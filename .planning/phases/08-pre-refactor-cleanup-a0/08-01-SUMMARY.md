---
phase: 08-pre-refactor-cleanup-a0
plan: 01
subsystem: blockstore/local
tags: [bug-fix, TD-02, TD-02a, goroutine-leak, lifecycle]
dependency_graph:
  requires: []
  provides:
    - "Deterministic FSStore.Start/Close lifecycle (no goroutine leak)."
  affects:
    - "Any caller of FSStore.Close() (now blocks until Start goroutine joins)."
tech_stack:
  added: []
  patterns:
    - "sync.Once-guarded close(channel) + sync.WaitGroup join on teardown."
key_files:
  created: []
  modified:
    - pkg/blockstore/local/fs/fs.go
    - pkg/blockstore/local/fs/fs_test.go
decisions:
  - "Manual runtime.NumGoroutine() snapshot used for leak detection — go.uber.org/goleak not a repo dependency (verified against go.mod)."
  - "Test uses a never-cancelled context.Background() so Close() is the ONLY goroutine-exit path — proves the join contract rather than relying on ctx cancellation."
  - "`done` channel is additive to existing ctx-cancellation path; both remain valid exit signals so existing callers who rely on ctx cancellation still work."
  - "Close() calls wg.Wait() *before* SyncFileBlocks/fdPool.CloseAll so we don't race the Start goroutine's final drain against the teardown sync."
metrics:
  duration: ~25 minutes
  completed: 2026-04-23
requirements: [TD-02]
commits:
  - 6f13cbd0 (fix(blockstore): join FSStore.Start goroutine on Close (TD-02a))
---

# Phase 08 Plan 01: FSStore goroutine-leak fix (TD-02a) Summary

Fix TD-02a: FSStore.Start() goroutine is now joined on Close() via a `done chan` + `sync.WaitGroup`, making teardown deterministic and eliminating a slow-leak DoS in long-lived processes.

## What changed

### `pkg/blockstore/local/fs/fs.go`

- Added three fields to `FSStore`:
  - `done chan struct{}` — signal channel closed exactly once by `Close()`.
  - `closeOnce sync.Once` — ensures `close(done)` runs only once (idempotent `Close()`).
  - `wg sync.WaitGroup` — join handle for the Start goroutine.
- `New()` now initializes `done = make(chan struct{})`. Other fields use zero values.
- `Start(ctx)` calls `bc.wg.Add(1)` before `go func`, and the goroutine:
  - Uses `defer bc.wg.Done()`.
  - Selects on both `<-ctx.Done()` AND `<-bc.done` (either triggers drain + return).
- `Close()`:
  - Sets `closedFlag`.
  - `closeOnce.Do(func() { close(bc.done) })`.
  - `bc.wg.Wait()` — **new**; blocks until the Start goroutine exits.
  - Proceeds with `SyncFileBlocks`, `fdPool.CloseAll()`, `readFDPool.CloseAll()` as before.
  - Idempotent: a second `Close()` is a no-op (Once already fired, wg already at zero).

### `pkg/blockstore/local/fs/fs_test.go`

- New test `TestFSStoreStartCloseNoGoroutineLeak`:
  - Warms up, samples `runtime.NumGoroutine()` as `before`.
  - Runs 20 `New → Start(background-ctx) → Close()` cycles with a never-cancelled parent context, so `Close()` is the *only* goroutine-exit path available.
  - Settles 100ms, GCs, samples `after`.
  - Asserts `after - before <= 2` (tolerance for unrelated test-runner noise).
- Added imports for `runtime` and `time`.

## Verification

| Gate | Command | Result |
|------|---------|--------|
| RED  | `go test -race -run TestFSStoreStartCloseNoGoroutineLeak ./pkg/blockstore/local/fs/...` (pre-fix) | FAIL — `delta=20` leaked goroutines across 20 cycles. |
| GREEN | same command (post-fix) | PASS |
| Full package tests | `go test -race ./pkg/blockstore/local/fs/...` | PASS (6.9s) |
| Whole-repo build | `go build ./...` | PASS |
| Lint | `go vet ./pkg/blockstore/...` | PASS |
| Signed commit | `git log -1 --show-signature` | Good RSA signature |
| Commit convention | `git log -1 --format=%s` | `fix(blockstore): join FSStore.Start goroutine on Close (TD-02a)` |
| No AI mentions | grep `claude code|co-authored-by` | none |

## Deviations from Plan

None — plan executed exactly as written. The two deviation candidates explicitly surfaced in `<read_first>` (goleak availability, test style) resolved mechanically:

- **goleak:** `grep "goleak\|go.uber.org"` in `go.mod` returned no matches, so I used the fallback `runtime.NumGoroutine()` + settle-sleep approach called out in the plan.
- **Test style:** Matched project convention (`TestX` top-level, `t.TempDir()`, `t.Fatalf` with context). No table-driven form needed — single behavior under test.

## Known Stubs

None.

## Threat Flags

None. The threat model in the plan lists `T-08-01-01` (DoS via goroutine leak) as `mitigate` — this commit is exactly that mitigation. No new trust boundaries, no new network surface, no auth context changes.

## Deferred Issues

None.

## TDD Gate Compliance

This is a `type=auto` task with `tdd=true`. Gate sequence verified locally:

1. **RED** — test added, run confirmed FAIL with `delta=20`, recorded in verification table above.
2. **GREEN** — fix applied, same test now PASS.
3. **REFACTOR** — none needed; implementation is minimal.

Per plan instructions (Step 4, D-11), test + fix landed as a *single* atomic commit so the commit is independently green under `go test -race ./...`. A separate test-only RED commit would have left `develop` failing, violating PROJECT.md's "each step must compile and pass all tests independently" constraint.

## Self-Check: PASSED

- FOUND: `pkg/blockstore/local/fs/fs.go` (modified — 3 new fields, Start wg-tracked, Close wg-joined)
- FOUND: `pkg/blockstore/local/fs/fs_test.go` (modified — `TestFSStoreStartCloseNoGoroutineLeak` added)
- FOUND: commit `6f13cbd0` in git log (signed, convention-compliant, no AI mentions)
- FOUND: `wg.Wait()` at `pkg/blockstore/local/fs/fs.go:195`
- FOUND: `done chan struct{}` at `pkg/blockstore/local/fs/fs.go:117`
