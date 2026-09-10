# PR body — adapter lifecycle: listener-ready release on shutdown + accept retry backoff

## Suggested PR title

adapter: release listener-ready waiters on shutdown and bound the accept retry loop

## What and why

Two lifecycle hardenings in the shared TCP accept loop (`pkg/adapter/base.go`), covering the
"3 core HIGHs, one root cause" adapter-lifecycle item:

### 1. `listenerReady` is released when shutdown fires without a bound listener

**Root cause:** `GetListenerAddr` blocks on the `listenerReady` channel, and that channel was
only closed by `ServeWithFactory` after a successful bind. Any shutdown path that fires
without a bound listener — `Stop()` called before `ServeWithFactory`, or an adapter that never
serves — left the channel open forever, hanging every waiter.

**Fix:** `initiateShutdown` now closes `listenerReady` when no listener was bound. The close is
guarded by a `listenerReadyClosed` flag checked under `listenerMu`, and the bind path performs
its store-and-close under the same critical section, so the channel closes exactly once
whichever path wins — a normal-Serve `Stop` cannot double-close.

**Invariants preserved:** the documented ordering invariant (`started` flipped before the ready
close on the bind path) is unchanged; `ListenerReady` still stays open on a bind *failure*, so
the control-plane start path can keep disambiguating a successful bind from a failed one by
racing the channel against the serve goroutine's return.

### 2. Bounded accept retry backoff

**Root cause:** the accept loop logged unexpected `Accept` errors and `continue`d with no
delay. A persistent failure — a process hitting its file-descriptor limit — spun the loop hot,
pinning a core.

**Fix:** after an unexpected error the loop sleeps 10ms, doubling per consecutive failure and
capped at 1s, reset to zero on a successful accept. The sleep is a select against the shutdown
channel, so it stays interruptible: a shutdown that lands mid-backoff unwinds immediately
instead of waiting out the sleep. The shutdown-detection branch itself is unchanged and still
returns without sleeping.

## Disclosed limitation (bind-failure close deliberately dropped)

An earlier draft also closed `listenerReady` on the bind-failure path. That was killed before
implementation: the control-plane start path
(`pkg/controlplane/runtime/adapters/service.go`, `awaitListener`) selects between
`ListenerReady()` (→ report the adapter as started) and the serve goroutine's return (→ report
the bind error). A bind-failure close would make both channels ready and randomize the select,
so roughly half of all failed binds would be reported as successes. The documented contract on
`ListenerReady` ("stays open when the bind fails") is what makes that disambiguation possible,
so it is preserved here unchanged.

Note: `GetListenerAddr` has no external callers (the real consumer seam is
`ListenerReady`/`awaitListener`), so the fix's user-visible effect is confined to tests and any
future caller of the accessor; the runtime start path was already correct.

## Verification

- `go build ./...` — OK
- `go vet ./...` — clean
- `gofmt -l` on both changed files — nothing printed
- `go test -race -count=1 ./pkg/adapter/` — ok (2.6s)
- `go test -count=1 ./...` — all packages ok

New tests:

- `TestBaseAdapter_GetListenerAddr_ReturnsAfterStopBeforeStart` — Stop before Serve releases a
  blocked `GetListenerAddr` with the empty address (the hang fix)
- `TestBaseAdapter_GetListenerAddr_ReturnsWhenServeNeverRuns` — the never-served shape
- `TestBaseAdapter_GetListenerAddr_ReturnsAddrAfterServe` — normal-Serve `Stop` does not
  double-close or corrupt the ready channel
- `TestBaseAdapter_AcceptBackoff_ExitsOnShutdown` — an always-failing non-shutdown Accept
  holds at most ~4 attempts in a 150ms window (hot-loop bound) and shutdown interrupts the
  backoff sleep promptly
- `TestBaseAdapter_AcceptBackoff_ResetsOnSuccess` — the failure streak resets after a
  successful accept

## Deferred cleanup

None. The `sync.Once`-style guard uses a plain bool under `listenerMu` rather than a second
`sync.Once` so the bind path's check-and-close stays in the same critical section as the
listener store.
