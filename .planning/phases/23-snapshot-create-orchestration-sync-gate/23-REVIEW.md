---
phase: 23-snapshot-create-orchestration-sync-gate
reviewed: 2026-05-28T00:00:00Z
depth: standard
files_reviewed: 21
files_reviewed_list:
  - pkg/blockstore/engine/engine.go
  - pkg/config/config.go
  - pkg/config/defaults.go
  - pkg/config/snapshot_test.go
  - pkg/controlplane/models/errors.go
  - pkg/controlplane/models/errors_test.go
  - pkg/controlplane/runtime/runtime.go
  - pkg/controlplane/runtime/snapshot.go
  - pkg/controlplane/runtime/snapshot_hold.go
  - pkg/controlplane/runtime/snapshot_hold_test.go
  - pkg/controlplane/runtime/snapshot_integration_test.go
  - pkg/controlplane/runtime/snapshot_lifecycle_test.go
  - pkg/controlplane/runtime/snapshot_test.go
  - pkg/controlplane/store/interface.go
  - pkg/controlplane/store/snapshots.go
  - pkg/snapshot/dump.go
  - pkg/snapshot/dump_test.go
  - pkg/snapshot/retry.go
  - pkg/snapshot/retry_test.go
  - pkg/snapshot/syncgate.go
  - pkg/snapshot/syncgate_test.go
findings:
  critical: 3
  warning: 9
  info: 3
  total: 15
status: issues_found
---

# Phase 23: Code Review Report

**Reviewed:** 2026-05-28
**Depth:** standard
**Files Reviewed:** 21
**Status:** issues_found

## Summary

Phase 23 lands the asynchronous CreateSnapshot orchestration, the per-share
in-flight goroutine registry, the on-disk-manifest GC hold filter (D-23-02),
the parallel sync-gate verifier, and the snapshot recovery sweep. The
overall shape is sound and the integration tests do a good job of pinning
down the happy paths and the lifecycle ordering with RemoveShare.

The two correctness problems worth blocking on are:

1. **Registration-vs-shutdown race in `registerSnapInFlight`** — the
   per-snap WaitGroup is incremented *after* `snapInFlightMu` is dropped,
   so a concurrent `cancelAndWaitInFlightSnaps` can see an empty entry,
   `wg.Wait()` returns immediately, RemoveShare proceeds to wipe the
   snapshots tree, and the goroutine (launched moments later) writes into
   a directory being deleted.

2. **`SnapshotConfig.Validate` is never called from the top-level
   `config.Validate`**, so the documented `[1, 256]` range gate on
   `snapshot.sync_gate_concurrency` is dead — operator typos like
   `5000` silently pass config load and are then coerced/used unchecked.

3. **Per-remote `SnapshotHoldProvider` instances each own a separate
   `sync.RWMutex`** — the D-23-04 delete-vs-read invariant is only
   provided *within one provider instance*. With multiple remotes each
   getting their own provider, `AcquireDeleteLock` on one does not block
   `HeldHashes` on the siblings; the GC mark phase can still race a
   row+manifest deletion. Not exploitable today because no caller wires
   `AcquireDeleteLock` yet, but the type-level guarantee is broken.

The remaining items are quality issues (sentinel labelling for cancel vs.
timeout vs. share-removed, unbounded `entry.cancels` growth, a
ready-flipped-to-failed race during shutdown, the silent
`RemoteDurable` flip failure swallowing, store-level retry surfacing the
raw unique-constraint error, etc.).

## Critical Issues

### CR-01: registerSnapInFlight has a TOCTOU between map insert and `wg.Add(1)`; concurrent RemoveShare wipes the share tree out from under the launching goroutine

**File:** `pkg/controlplane/runtime/snapshot.go:252-273`
**Issue:**

`registerSnapInFlight` releases `snapInFlightMu` *between* inserting the
share entry and calling `entry.wg.Add(1)`:

```go
r.snapInFlightMu.Lock()
entry, ok := r.snapInFlight[shareName]
if !ok {
    entry = &snapInFlight{ done: make(map[string]chan snapResult) }
    r.snapInFlight[shareName] = entry
}
r.snapInFlightMu.Unlock()

entry.mu.Lock()
entry.cancels = append(entry.cancels, cancel)
entry.done[snapID] = doneCh
entry.wg.Add(1)
entry.mu.Unlock()
```

Interleaving:

1. CreateSnapshot (G1): inserts the empty `entry` under `snapInFlightMu`,
   drops the lock.
2. RemoveShare (G2): `cancelAndWaitInFlightSnaps` takes
   `snapInFlightMu`, sees the entry, deletes it from the map, drops the
   lock, takes `entry.mu`, copies the (still empty) `cancels`, drops
   `entry.mu`, calls `entry.wg.Wait()` — returns immediately because
   nothing has called `Add` yet.
3. RemoveShare returns to `sharesSvc.RemoveShare(name)` which runs the
   Phase 22 D-15 hook and wipes `<localStoreDir>/snapshots/`.
4. G1: takes `entry.mu`, registers `cancel` + `done` + `wg.Add(1)`,
   launches `runSnapshotOrchestration`.
5. G1 goroutine: `WriteMetadataDumpAtomic` opens
   `<localStoreDir>/snapshots/<snapID>/metadata.dump.tmp` — directory
   is gone (best case ENOENT, then `failSnap`; worst case the tree was
   re-created by a follow-up `AddShare` and we corrupt a stranger's
   snapshot dir).

The race is not theoretical — it's the precise window the comment on
`cancelAndWaitInFlightSnaps` ("snapshots/ tree wipe (Phase 22 D-15)")
warns about, and the lock-discipline comment claims is closed. The fact
that `entry.wg.Add` happens *after* `snapInFlightMu` is released defeats
that claim.

Even without the goroutine launch, the sibling defect is that the snap's
DB row is in `state=creating` with no orchestration runner; only the
startup-recovery sweep (D-23-18) at the next process restart will
reconcile it.

**Fix:**

Either hold `snapInFlightMu` across `wg.Add(1)` (simplest), or do the
`wg.Add(1)` *under* `snapInFlightMu` so `cancelAndWaitInFlightSnaps`
either sees an entry with the Add already in or does not see the entry
at all:

```go
func (r *Runtime) registerSnapInFlight(shareName, snapID string) (context.Context, chan snapResult, *snapInFlight) {
    childCtx, cancel := context.WithCancel(r.runtimeCtx)
    doneCh := make(chan snapResult, 1)

    r.snapInFlightMu.Lock()
    entry, ok := r.snapInFlight[shareName]
    if !ok {
        entry = &snapInFlight{done: make(map[string]chan snapResult)}
        r.snapInFlight[shareName] = entry
    }
    // wg.Add MUST happen under snapInFlightMu so cancelAndWaitInFlightSnaps
    // cannot snapshot+drain the entry between us inserting it and us
    // registering the goroutine that will Done it.
    entry.mu.Lock()
    entry.cancels = append(entry.cancels, cancel)
    entry.done[snapID] = doneCh
    entry.wg.Add(1)
    entry.mu.Unlock()
    r.snapInFlightMu.Unlock()

    return childCtx, doneCh, entry
}
```

Mirror this in `cancelAndWaitInFlightSnaps`: take `snapInFlightMu`,
remove the entry, drop `snapInFlightMu`, *then* take `entry.mu` to
snapshot cancels (already done correctly).

### CR-02: `SnapshotConfig.Validate` is never invoked by `config.Validate`; the `[1, 256]` range gate is dead

**File:** `pkg/config/validation.go:21-31`, `pkg/config/config.go:209-237`
**Issue:**

`Validate(cfg)` only calls `validate.Struct(cfg)` (struct tags) and
`cfg.Blockstore.Validate()`. Neither `cfg.Snapshot.Validate()`,
`cfg.Syncer.Validate()` nor `cfg.GC.Validate()` is invoked, so the
documented Phase 23 range check

```go
if c.SyncGateConcurrency < 1 || c.SyncGateConcurrency > 256 {
    return fmt.Errorf("snapshot.sync_gate_concurrency must be in [1, 256] (got %d)", c.SyncGateConcurrency)
}
```

is unreachable from `Load`/`MustLoad`. An operator writing
`snapshot: { sync_gate_concurrency: 5000 }` in `config.yaml` sees the
server boot cleanly with that 5000-way Head() fan-out against S3,
contrary to the gate's own justification ("higher values risk
overwhelming restrictive remote endpoints").

Defense-in-depth in `Runtime.SetSnapshotDefaults` only handles `<= 0`,
not over-max, so the bad value reaches `VerifyRemoteDurability` intact.

The unit test `TestSnapshotConfig_Validate_RangeBounds` exercises the
method directly — and passes — which gives a false sense of coverage:
nothing actually calls the method on the config-load path.

**Fix:**

Add the snapshot validator (and likely Syncer + GC, but those are out
of phase scope) to `config.Validate`:

```go
func Validate(cfg *Config) error {
    if err := validate.Struct(cfg); err != nil {
        return formatValidationError(err)
    }
    if err := cfg.Blockstore.Validate(); err != nil {
        return err
    }
    if err := cfg.Snapshot.Validate(); err != nil {
        return err
    }
    return nil
}
```

Add a regression test in `pkg/config` that loads a YAML with
`sync_gate_concurrency: 300` and asserts `Load` errors with a non-nil
config-validation error — this is what would have caught the gap.

### CR-03: `SnapshotHoldProvider.mu` is per-provider-instance, so D-23-04 is not actually enforced across the per-remote scopes

**File:** `pkg/controlplane/runtime/snapshot_hold.go:33-44, 131-144`
**Issue:**

`snapshotHoldForRemote` builds a fresh `SnapshotHoldProvider` value
on every call:

```go
func (r *Runtime) snapshotHoldForRemote(shareNames []string) engine.HoldProvider {
    return &SnapshotHoldProvider{
        rt:     r,
        shares: append([]string(nil), shareNames...),
    }
}
```

Each instance carries its own zero-value `sync.RWMutex`. So
`AcquireDeleteLock()` on the provider handed to remote A does *not*
block `HeldHashes` calls on the provider handed to remote B, even when
both providers' `shares` lists overlap (or are identical, in the
common "shares share a remote endpoint config" case noted in
`runtime.go`'s "Remote stores are ref-counted").

The race that D-23-04 is supposed to close — concurrent snapshot
delete (row + manifest dir removal) vs. GC mark phase reading the
manifest — is therefore still reachable across providers.

This is not currently exploitable: no production caller wires
`AcquireDeleteLock` yet (the orchestration delete path is scheduled for
plans 23-04/05 follow-up). But the type-level guarantee is broken
*today*, the race test
`TestSnapshotHoldProvider_DeleteVsHeldHashes_Race` passes only because
it uses a single provider instance, and the regression will be silent
when the delete path lands.

**Fix:**

Lift the mutex onto the Runtime (or onto a per-share lock keyed by
share name) so every provider for the same share-scope shares one
lock:

```go
type Runtime struct {
    ...
    snapshotHoldMu sync.RWMutex  // protects all SnapshotHoldProvider instances
}

func (p *SnapshotHoldProvider) HeldHashes(...) error {
    p.rt.snapshotHoldMu.RLock()
    defer p.rt.snapshotHoldMu.RUnlock()
    ...
}

func (p *SnapshotHoldProvider) AcquireDeleteLock() (release func()) {
    p.rt.snapshotHoldMu.Lock()
    return p.rt.snapshotHoldMu.Unlock
}
```

Extend the race test to spawn two providers (different `shareNames`
slices but with one share in common) and assert the delete lock on one
provider serializes reads on the other.

## Warnings

### WR-01: A shutdown-cancelled `UpdateSnapshotState(ready)` after a successful drain+verify is misreported as `state=failed`

**File:** `pkg/controlplane/runtime/snapshot.go:471-478`
**Issue:**

Step 6 of `runSnapshotOrchestration` does:

```go
if err := r.store.UpdateSnapshotState(ctx, shareName, snapID, models.StateReady); err != nil {
    r.failSnap(shareName, snapID)
    terminalErr = fmt.Errorf("snapshot create %s: flip ready: %w: %v", ...)
    ...
}
```

`ctx` is the child of `r.runtimeCtx`. If Shutdown races *after* a
successful Backup + DrainAllUploads + VerifyRemoteDurability but
*before* the ready flip, the conditional UPDATE inherits ctx.Canceled
and the row is flipped to `failed`. The snapshot is in fact durable
(all hashes confirmed present on remote), but operators see `failed`
and have to re-run the entire pipeline.

`failSnap` deliberately uses `context.Background()` so the failed flip
always lands — which compounds the misreport.

Mitigations:

- The retry path is idempotent, so the recovery is cheap.
- The wrapped sentinel is `ErrSnapshotBackupFailed`, not
  `ErrSnapshotVerifyFailed`, which obscures the actual cause for
  operators triaging.

**Fix:**

For the post-verify ready flip specifically, prefer
`context.Background()` (or a `context.WithoutCancel(ctx)` wrapper) so
that work which has *already* produced a durable artifact is recorded
honestly. Mirror the same treatment for `UpdateSnapshotDurable` (it
already accepts the same risk silently — see WR-02).

### WR-02: `UpdateSnapshotDurable` failures are silently swallowed, leaving `state=ready, RemoteDurable=false` indistinguishable from a NoSyncGate run

**File:** `pkg/controlplane/runtime/snapshot.go:388-393, 479-485`
**Issue:**

Both the NoSyncGate and the verify-success paths set
`RemoteDurable` after the state flip, then *log and continue* on
failure:

```go
if err := r.store.UpdateSnapshotDurable(ctx, shareName, snapID, true); err != nil {
    logger.Error("snapshot create: set remote_durable=true failed", ...)
}
```

A snapshot that has successfully drained and verified but whose
`UpdateSnapshotDurable` write failed is observationally identical to a
snapshot that the operator deliberately created with `--no-sync-gate`.
Phase 24 restore (per the docstring) "reads RemoteDurable=false and
refuses (or requires --force)". That contract is broken by any flake in
the durability flip — and the comment's "caller can re-run the verify
path explicitly if needed" assumes a verify-only endpoint that does
not appear to exist yet.

**Fix:**

Either:

- Retry the durability flip a bounded number of times with
  `context.Background()` (parallels CR-02 above), and on persistent
  failure flip the row to `failed` rather than `ready` — better to
  re-do the snapshot than to lie about durability.
- Or invert the order: set `RemoteDurable=true` *first*, then flip
  state to `ready`. Then a partial failure leaves the row in `creating`
  (caught by startup recovery → `failed`) rather than `ready` +
  not-durable.

### WR-03: Drain + state-flip failures get a misleading `ErrSnapshotDrainTimeout` label on cancel-via-shutdown

**File:** `pkg/controlplane/runtime/snapshot.go:417-419, 450-452`
**Issue:**

The drain failure branch maps both `context.Canceled` and
`context.DeadlineExceeded` to `models.ErrSnapshotDrainTimeout`. A
goroutine cancelled by RemoveShare or Shutdown is fundamentally not a
"timeout" — there is no deadline; the operator triggered it. Operators
chasing `ErrSnapshotDrainTimeout` alerts will be looking for
slow-remote symptoms that aren't there.

A similar smell shows up earlier when the share's block store cannot
be found:

```go
terminalErr = fmt.Errorf("snapshot create %s: no block store for share %q: %w",
    snapID, shareName, models.ErrSnapshotVerifyFailed)
```

Share removal during orchestration is mapped to *verify failed*. That
also obscures triage.

**Fix:**

Differentiate the sentinels:

- Add `models.ErrSnapshotCancelled` (or reuse `context.Canceled` via
  `errors.Is`) for orchestration cancellation, distinct from drain
  timeout.
- Add `models.ErrSnapshotShareGone` (or similar) for the
  share-removed-mid-orchestration window in step 4.

At minimum, log the underlying `err` at WARN with a "cancelled"
qualifier when the cause is ctx cancellation, so operators can grep.

### WR-04: `entry.cancels` grows unboundedly per share lifetime

**File:** `pkg/controlplane/runtime/snapshot.go:267, 280-284`
**Issue:**

`registerSnapInFlight` appends every snapshot's `cancel` func into
`entry.cancels`, and `unregisterSnap` deletes only the per-snap
done-channel entry, never the matching cancel. A share that creates N
snapshots over its lifetime accumulates N CancelFuncs even though
N-many of them are already long-since-cancelled and orphaned.

Each `context.CancelFunc` retains a reference back to its `cancelCtx`
which retains the entire `runtimeCtx` parent chain — the leak is small
but real, and `cancelAndWaitInFlightSnaps` will re-call every stale
cancel during share removal (idempotent but wasted work).

**Fix:**

Store the cancel func on the entry keyed by snapID (alongside the done
channel), and have `unregisterSnap` remove it. Then
`cancelAndWaitInFlightSnaps` iterates the map values instead of a
slice:

```go
type snapInFlight struct {
    wg     sync.WaitGroup
    snaps  map[string]snapEntry // keyed by snapID
    mu     sync.Mutex
}
type snapEntry struct {
    cancel context.CancelFunc
    done   chan snapResult
}
```

### WR-05: Retry path returns raw unique-constraint errors instead of `ErrSnapshotStateConflict`

**File:** `pkg/controlplane/store/snapshots.go:88-101`
**Issue:**

`UpdateSnapshotState` returns `res.Error` verbatim when the conditional
UPDATE fails. If the UPDATE is the `failed → creating` retry flip and
another snapshot for the share is already in `creating` (concurrent
`CreateSnapshot` race), the partial unique index
`idx_share_creating` rejects the UPDATE with a database-level unique-
constraint error. The caller in `CreateSnapshot` (retry path) then
returns:

```go
return "", fmt.Errorf("snapshot retry: flip failed->creating for %q: %w",
    opts.RetryOf, uerr)
```

The wrapping preserves the GORM/SQLite error string but loses the
`models.ErrSnapshotStateConflict` sentinel — so callers' `errors.Is`
checks against the documented sentinel fail.

**Fix:**

Mirror the `CreateSnapshot` store helper's pattern — check
`isUniqueConstraintError(err)` and translate before returning:

```go
if res.Error != nil {
    if isUniqueConstraintError(res.Error) && state == models.StateCreating {
        return models.ErrSnapshotStateConflict
    }
    return res.Error
}
```

### WR-06: `WaitForSnapshot` discards the orchestration result if `GetSnapshot` fails after a successful chan-receive

**File:** `pkg/controlplane/runtime/snapshot.go:237-243`
**Issue:**

The flow is "receive orchestration error from done chan → call
`GetSnapshot(ctx, …)`". If ctx is cancelled (or the DB is briefly
unavailable) between the two steps, the `gerr` short-circuits and the
caller never sees the carefully-captured `orchErr`:

```go
snap, gerr := r.store.GetSnapshot(ctx, shareName, snapID)
if gerr != nil {
    return nil, gerr
}
return snap, orchErr
```

That swallows the most informative diagnostic from the orchestration
goroutine.

**Fix:**

If `orchErr != nil`, prefer it over `gerr` (and log `gerr`); or use
`errors.Join(orchErr, gerr)` so callers can `errors.Is` against either
sentinel.

### WR-07: `runSnapshotOrchestration` deferred `doneCh <- snapResult{…}` will panic if `doneCh` is somehow nil

**File:** `pkg/controlplane/runtime/snapshot.go:300-308`
**Issue:**

The deferred cleanup unconditionally sends and then closes
`doneCh`. The channel is constructed by `registerSnapInFlight` with
cap 1, so a single send is always non-blocking, and the close is
single-shot — that part is fine.

The risk is more about future refactors: if any caller ever wires
`runSnapshotOrchestration` with a nil `doneCh` (a test fake, a refactor
where doneCh is allocated lazily, etc.), the deferred block panics on
nil-channel send. There's no defensive guard.

This is mostly a defensive-coding nit, but the cost is one nil-check.

**Fix:**

```go
defer func() {
    if doneCh != nil {
        doneCh <- snapResult{err: terminalErr}
        close(doneCh)
    }
    r.unregisterSnap(shareName, snapID, entry)
    entry.wg.Done()
}()
```

Order also matters: `entry.wg.Done()` should be the very last action
so it always fires even if `doneCh` operations panic (cleaner crash
recovery for `cancelAndWaitInFlightSnaps`).

### WR-08: `recoverOrphanedSnapshots` accumulates errors but `firstErr` may mask correlated failures

**File:** `pkg/controlplane/runtime/snapshot.go:646-700`
**Issue:**

The recovery sweep captures only the *first* error across all
per-share / per-snap failures, returning it from `Serve`. If a
single share has 50 orphan rows and all 50 flips fail (DB outage), the
caller sees only the first one, and the structured log emits 50
separate Error lines without correlation.

Worse, the function is called once from `Serve` and any error short-
circuits "non-fatal continue" — but the bookkeeping uses
`errs int` (line 681) that is never surfaced (only `firstErr` is
returned).

**Fix:**

- Surface `errs` in the log line (`logger.Info("snapshot recovery:
  complete", "errors", errs, "first_err", firstErr)` — the current
  call already does this for `errs`).
- Consider `errors.Join` over the collected error slice when the
  caller can usefully iterate them, instead of a single `firstErr`.

Low risk because the docstring already calls this out, but the
`firstErr`-only contract is fragile.

### WR-09: `var _ = shares.ErrShareNotFound` is dead anchor code

**File:** `pkg/controlplane/runtime/snapshot.go:705`
**Issue:**

The file already uses `r.sharesSvc.LocalStoreDir` (in
`CreateSnapshot`) which forces the `shares` import. The
`var _ = shares.ErrShareNotFound` line is therefore a no-op anchor with
a defensive comment ("Keep the alias here in case future refactors
prune"). Per the CLAUDE.md "less is more" guidance for v0.16+ refactors
("delete legacy eagerly, no compat shims"), the anchor is exactly the
kind of speculative scaffolding to delete.

**Fix:**

Delete the line and the docstring above it. The import will hold on
its own; if a future refactor removes the last real use, `go vet` will
flag the unused import in the same change-set.

## Info

### IN-01: `streamManifest` ignores `Close` errors silently

**File:** `pkg/controlplane/runtime/snapshot_hold.go:108-123`
**Issue:**

`defer func() { _ = f.Close() }()` discards close errors. For a
read-only manifest file the Close error is almost certainly benign,
but the pattern is inconsistent with `WriteMetadataDumpAtomic`
(which surfaces Close errors). A reviewer following the trail will
wonder why one place cares and the other does not.

**Fix:**

Either document the asymmetry inline ("read-only Close errors are
discarded — there is nothing to roll back") or log at Debug for
operator visibility.

### IN-02: `findRowCoveringOffset` is `O(N)` per byte-range read

**File:** `pkg/blockstore/engine/engine.go:1146-1156, 1205-1219`
**Issue:**

This is pre-existing code untouched by Phase 23, flagged only because
it is on the read path used during the post-snapshot restore window:
`readLocalByHash` calls `findRowCoveringOffset` once per chunk, and
each call walks the entire `rows` slice. For a 4 GiB file with ~1000
FastCDC rows, a full sequential read is roughly `1000 * 1000 / 2`
linear scans.

Out of v1 scope per the review rubric (performance), noted for
posterity in case the post-restore read path becomes a hot path in
later phases.

**Fix:**

Replace the linear walk with a binary search keyed on the parsed
`absOffset`, or sort the rows once and cache the sorted view.

### IN-03: Magic literal `0o750` for snapshot dirs / `0o600` for temp files; permission constants would aid auditability

**File:** `pkg/controlplane/runtime/snapshot.go:134`, `pkg/snapshot/dump.go:35`
**Issue:**

The two umasks are correct and consistent with Phase 22, but they
appear as bare literals. A reader auditing for permission leaks has to
re-derive intent at every call site.

**Fix:**

Hoist into named constants in `pkg/snapshot/` (e.g.,
`SnapshotDirMode = 0o750`, `SnapshotFileMode = 0o600`) so the
intent is grep-able and any future loosening is a single-site change.

---

_Reviewed: 2026-05-28_
_Reviewer: Claude (gsd-code-reviewer)_
_Depth: standard_
