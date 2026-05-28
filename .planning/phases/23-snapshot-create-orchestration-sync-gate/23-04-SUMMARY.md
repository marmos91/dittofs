---
phase: 23-snapshot-create-orchestration-sync-gate
plan: 04
subsystem: controlplane/runtime
tags: [snapshot, orchestration, async, sync-gate, registry]
requires:
  - .planning/phases/23-snapshot-create-orchestration-sync-gate/23-01-SUMMARY.md
  - .planning/phases/23-snapshot-create-orchestration-sync-gate/23-02-SUMMARY.md
  - .planning/phases/23-snapshot-create-orchestration-sync-gate/23-03-SUMMARY.md
provides:
  - "Runtime.CreateSnapshot(ctx, shareName, CreateSnapshotOpts) (snapID, error)"
  - "CreateSnapshotOpts{NoSyncGate, RetryOf}"
  - "Runtime.SetSnapshotDefaults / snapshotDefaults() (D-23-22 wiring)"
  - "Runtime.snapInFlight registry + snapResult chan (consumed by plans 23-05 + 23-06)"
  - "snapshot.WriteMetadataDumpAtomic + snapshot.ValidateRetryTarget pure helpers"
  - "BlockStore.RemoteStore() production accessor"
  - "SnapshotStore.UpdateSnapshotDurable on interface + GORM impl"
affects:
  - "Phase 23 plan 23-05 (RemoveShare/Shutdown integration consumes snapInFlight)"
  - "Phase 23 plan 23-06 (WaitForSnapshot consumes per-snap doneCh)"
  - "Phase 25 REST handler (errors.Is on the wrapped sentinels)"
tech_stack:
  added: []
  patterns:
    - "Centralized per-share goroutine registry (model: shares.Service registry)"
    - "Child ctx derived from long-lived runtimeCtx (NOT caller ctx) — D-23-17"
    - "Buffered (cap=1) per-snap result chan carrying snapResult{err error} for poll-free observation"
    - "Atomic temp+fsync+rename via WriteMetadataDumpAtomic (mirrors WriteManifestAtomic)"
    - "State-flip helper failSnap uses context.Background so cancelled parent ctx still releases the partial-unique-index slot"
key_files:
  created:
    - pkg/controlplane/runtime/snapshot.go
    - pkg/snapshot/dump.go
    - pkg/snapshot/dump_test.go
    - pkg/snapshot/retry.go
    - pkg/snapshot/retry_test.go
  modified:
    - pkg/controlplane/runtime/runtime.go
    - pkg/controlplane/store/interface.go
    - pkg/controlplane/store/snapshots.go
    - pkg/blockstore/engine/engine.go
decisions:
  - "D-23-01 honored: state=creating row inserted BEFORE any I/O"
  - "D-23-03 honored: ready+RemoteDurable=true only after VerifyRemoteDurability returns nil"
  - "D-23-05 honored: one drain+re-verify retry on ErrBlockNotFound; second miss fails"
  - "D-23-09 honored: failed-state rows retain metadata.dump + manifest.hashes on disk"
  - "D-23-10 honored: RetryOf reuses ID + dir; ValidateRetryTarget rejects non-failed"
  - "D-23-11 honored: NoSyncGate skips drain+verify; final=ready+RemoteDurable=false"
  - "D-23-12 honored: all 5 sentinels reachable + wrapped via fmt.Errorf %w"
  - "D-23-13 honored: CreateSnapshot returns (snapID, nil) immediately after row+dir"
  - "D-23-15 honored: plain-struct CreateSnapshotOpts, no functional options"
  - "D-23-16 honored: slog Debug-on-entry + Info-on-completion at every step"
  - "D-23-17 honored: centralized snapInFlight on Runtime + child ctx from runtimeCtx"
  - "D-23-22 honored: snapshotDefaults().SyncGateConcurrency wired to VerifyRemoteDurability"
metrics:
  duration: ~35 minutes
  completed: 2026-05-28
  tasks: 3/3
  commits: 4 (1 RED + 3 GREEN/feat)
---

# Phase 23 Plan 04: Runtime Snapshot Create Orchestration Summary

Composed the wave-1 surfaces (sync gate, sentinels, hold filter) into the
end-to-end async snapshot orchestration entrypoint. Synchronous call inserts
the `state=creating` row + on-disk dir, registers the goroutine, returns
`(snapID, nil)`. A background goroutine runs the backup → manifest → drain
→ verify → ready/failed pipeline derived from a long-lived `runtimeCtx`
so adapter teardown does not abort in-flight snapshots.

## Signatures Landed

```go
// pkg/controlplane/runtime/snapshot.go
type CreateSnapshotOpts struct {
    NoSyncGate bool   // D-23-11
    RetryOf    string // D-23-10
}
func (r *Runtime) CreateSnapshot(
    ctx context.Context,
    shareName string,
    opts CreateSnapshotOpts,
) (snapID string, err error)

// pkg/controlplane/runtime/runtime.go (extensions)
type SnapshotDefaults struct{ SyncGateConcurrency int }
func (r *Runtime) SetSnapshotDefaults(d SnapshotDefaults)
type snapInFlight struct{ wg sync.WaitGroup; cancels []context.CancelFunc;
                          done map[string]chan snapResult; mu sync.Mutex }
type snapResult struct{ err error }

// pkg/controlplane/store/{interface,snapshots}.go
UpdateSnapshotDurable(ctx, shareName, id, durable bool) error

// pkg/snapshot/dump.go
func WriteMetadataDumpAtomic(
    path string,
    write func(io.Writer) (*blockstore.HashSet, error),
) (*blockstore.HashSet, error)

// pkg/snapshot/retry.go
func ValidateRetryTarget(snap *models.Snapshot) error

// pkg/blockstore/engine/engine.go
func (bs *BlockStore) RemoteStore() remote.RemoteStore
```

## Registry Layout (D-23-17)

```
Runtime.snapInFlight  map[shareName]*snapInFlight   (guarded by snapInFlightMu)
                      │
                      └─ snapInFlight
                         ├─ wg      sync.WaitGroup
                         ├─ cancels []context.CancelFunc   (one per launched snap)
                         ├─ done    map[snapID]chan snapResult  (buffered cap=1)
                         └─ mu      sync.Mutex             (guards the slice + map)
```

`registerSnapInFlight` allocates/reuses the share entry, derives
`childCtx := context.WithCancel(r.runtimeCtx)` (NOT the caller's ctx),
appends `cancel`, makes `chan snapResult` (cap 1), `wg.Add(1)`.
`unregisterSnap` removes the per-snap done entry only; the share entry
is left in place even when empty so plan 23-05 / Shutdown can enumerate
and `wg.Wait` without re-allocation chatter.

## Orchestration Pipeline (goroutine body)

| Step | Action | Sentinel on failure |
|------|--------|---------------------|
| 1 | `WriteMetadataDumpAtomic(dumpPath, backupable.Backup(ctx, w))` | `ErrSnapshotBackupFailed` |
| 2 | `WriteManifestAtomic(manifestPath, hashSet)` | `ErrSnapshotBackupFailed` |
| 3a | (`NoSyncGate=true`) `UpdateSnapshotState(ready) + UpdateSnapshotDurable(false)` → return | `ErrSnapshotBackupFailed` on state flip fail |
| 3b | (else) continue to drain |  |
| 4 | `bs.DrainAllUploads(ctx)` | `ErrSnapshotDrainTimeout` if ctx; else `ErrSnapshotBackupFailed` |
| 5 | `VerifyRemoteDurability(ctx, bs.RemoteStore(), hashSet, concurrency)`; on `ErrBlockNotFound` → one `DrainAllUploads` + re-verify | `ErrSnapshotVerifyFailed` (on second miss) |
| 6 | `UpdateSnapshotState(ready) + UpdateSnapshotDurable(true)` | `ErrSnapshotBackupFailed` on state flip fail |

Every step: `slog.Debug` on entry + `slog.Info` on completion with
`snapshot_id` + `share` plus per-step keys (`dump_path`, `manifest_count`,
`verify_concurrency`, `final_state`, `remote_durable`).

## NoSyncGate + RetryOf Paths

**NoSyncGate (D-23-11):** Step 3a short-circuits past drain + verify.
Final state = ready, RemoteDurable=false. The hold filter from plan 23-03
(D-23-02 manifest-on-disk) still protects local blocks from GC, so the
operator can return later to verify or retry. Phase 24 restore reads
`RemoteDurable=false` and refuses unless `--force` is supplied.

**RetryOf (D-23-10):** Synchronous validation order:
1. `r.store.GetSnapshot(ctx, share, opts.RetryOf)` — `ErrSnapshotNotFound`
   maps to wrapped `ErrSnapshotRetryTargetNotFound`.
2. `snapshot.ValidateRetryTarget(existing)` — returns
   `ErrSnapshotRetryTargetNotFailed` for any state ≠ failed.
3. `UpdateSnapshotState(creating)` — flips `failed → creating`. The Phase
   22 idx_share_creating partial unique index then guards against a
   second concurrent retry against the same ID.

The reused snapshot dir is re-occupied — `os.MkdirAll(dir, 0o750)` is
idempotent — and `WriteManifestAtomic` overwrites `manifest.hashes`
atomically.

## slog Key Set

Mandatory on every line: `snapshot_id`, `share`. Step-specific:

| Step | Keys |
|------|------|
| accept | `no_sync_gate`, `retry_of` |
| backup start | `dump_path` |
| backup complete | `manifest_count` |
| manifest written | `manifest_count` |
| drain start/complete | (none extra) |
| verify start | `verify_concurrency` |
| verify miss retry | `first_error` |
| ready (sync gate skipped) | `final_state=ready`, `remote_durable=false` |
| ready | `manifest_count`, `verify_concurrency`, `final_state=ready`, `remote_durable=true` |
| failure paths | `error` |

Matches Phase 22's snapshot logging style (`snapshot_hold.go` debug line
already uses `snapshot_id` + `share`).

## snapResult Chan-Carries-Error Design (iteration-1 revision per D-23-19 alignment)

The per-snap `chan snapResult` (buffered cap 1) carries `snapResult{err
error}` rather than a bare `chan struct{}`. Rationale per the
iteration-1 revision recorded in 23-04-PLAN.md interfaces block: plan
23-06's `WaitForSnapshot` should be able to return the wrapped sentinel
directly via `errors.Is` without needing a DB column to round-trip the
error. The deferred-cleanup pattern is:

```go
defer func() {
    doneCh <- snapResult{err: terminalErr}  // non-blocking, cap=1
    close(doneCh)
    r.unregisterSnap(shareName, snapID, entry)
    entry.wg.Done()
}()
```

A closed channel still drains the buffered result, so a late-arriving
`WaitForSnapshot` subscriber sees the result and then the closure.

## failSnap Uses `context.Background`

The most common reason orchestration bails out is a cancelled parent
ctx (Runtime shutdown, caller deadline, plan 23-05 RemoveShare). Using
that same ctx for the failed-state flip would silently leave the row in
`state='creating'`, blocking the next attempt via the partial unique
index until the next startup-recovery sweep (plan 23-05). `failSnap`
deliberately uses `context.Background` so the row is released
immediately. If the flip itself fails (e.g., DB unavailable), the
wrapped sentinel posted on `doneCh` is still authoritative, and the
startup-recovery scan reconciles on the next restart.

## Tasks Completed

| Task | Name | Commit |
|------|------|--------|
| 1 | Runtime struct + UpdateSnapshotDurable + SnapshotDefaults | `ab28d748` |
| 2a (RED) | failing tests for WriteMetadataDumpAtomic + ValidateRetryTarget | `bf8d7e55` (test only) |
| 2b (GREEN) | dump.go + retry.go + RemoteStore() accessor | `c602b849` |
| 3 | Runtime.CreateSnapshot orchestration goroutine | `e2de85fa` |

(Commit hashes from `git log --oneline --no-decorate -8`.)

## Verification

```
$ go build ./...                                            # clean
$ go vet ./pkg/controlplane/runtime/... ./pkg/snapshot/...  # clean
$ gofmt -s -l pkg/controlplane/runtime/snapshot.go pkg/snapshot/dump.go \
              pkg/snapshot/retry.go pkg/blockstore/engine/engine.go     # no output
$ go test ./pkg/snapshot/... -run "TestWriteMetadataDumpAtomic|TestValidateRetryTarget" \
          -race -count=1                                    # PASS
$ go test ./pkg/controlplane/runtime/... -count=1 -short    # PASS (existing tests)
$ go test ./pkg/controlplane/store/... -count=1 -short      # PASS
```

Plan `<verification>` block (10 greps):

| Check | Result |
|-------|--------|
| `func (r *Runtime) CreateSnapshot` signature | line 65 |
| `type CreateSnapshotOpts struct` | line 24 |
| `type snapInFlight struct` / `type snapResult struct` | runtime.go:676 + 685 |
| `snapInFlight map[string]*snapInFlight` field | runtime.go:75 |
| `chan snapResult` (per-snap) | runtime.go:683 + snapshot.go (multiple) |
| `runtimeCtx`/`runtimeCancel` | runtime.go:85-86, 117 |
| `UpdateSnapshotDurable` on interface + impl | interface.go:488 + snapshots.go:127 |
| `func (bs *BlockStore) RemoteStore` | engine.go:878 |
| 5 sentinels referenced from snapshot.go | 20 references (>= 5 ok) |
| composed calls (Verify/WriteManifest/WriteMetadataDump) | snapshot.go:250, 283, 363, 381 |
| `bs.DrainAllUploads` + `bs.RemoteStore()` | snapshot.go:336, 350, 369 |
| `snapshotDefaults().SyncGateConcurrency` | snapshot.go:359 |

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 2 - Critical] Empty-engine HashSet defensive nil-check before manifest write**

- **Found during:** Task 3 implementation
- **Issue:** `Backupable.Backup` may legitimately return a nil `*HashSet`
  for an empty engine (no files yet). `WriteManifestAtomic` panics on
  `nil.Sorted()`.
- **Fix:** After backup, if `hashSet == nil` substitute
  `blockstore.NewHashSet(0)`. Manifest file is then a 0-byte file, which
  the plan-03 D-23-02 hold filter recognizes as "manifest present" =
  held (empty held set, but consistent semantics).
- **Files modified:** `pkg/controlplane/runtime/snapshot.go`
- **Commit:** rolled into `e2de85fa`

**2. [Rule 2 - Critical] failSnap uses context.Background**

- **Found during:** Task 3 implementation, while reasoning about
  D-23-09 + D-23-01 invariants
- **Issue:** If orchestration bails out because the parent ctx was
  cancelled (very common — Shutdown, RemoveShare, caller deadline), the
  plan's natural-reading implementation `UpdateSnapshotState(ctx, ...,
  StateFailed)` would also fail on the cancelled ctx, leaving the row
  in state=creating. The Phase 22 partial unique index then blocks the
  next attempt until the startup-recovery sweep on next restart.
- **Fix:** Introduced private `failSnap` helper that uses
  `context.Background` so the state release is independent of the
  orchestration-ctx cancellation. Error from the flip is logged (not
  re-thrown — the doneCh sentinel is still authoritative).
- **Files modified:** `pkg/controlplane/runtime/snapshot.go`
- **Commit:** rolled into `e2de85fa`

**3. [Rule 1 - Bug] State copy after retry-flip**

- **Found during:** Task 3 implementation
- **Issue:** On the RetryOf path the fetched `existing.State` is still
  `failed` after `UpdateSnapshotState(creating)` because we don't
  re-read the row. Downstream code that inspects `snap.State` would see
  stale data.
- **Fix:** `snap.State = models.StateCreating` immediately after the
  flip succeeds. Same value the DB now holds.
- **Files modified:** `pkg/controlplane/runtime/snapshot.go`
- **Commit:** rolled into `e2de85fa`

### Architectural Changes

None.

## Plan Choices Made (Planner Discretion)

- **Sub-method `runSnapshotOrchestration` (vs. inline body in
  CreateSnapshot):** kept the synchronous-phase body small and
  readable in `CreateSnapshot` (≈ 75 LoC including comments) and
  factored the goroutine body into a separate method
  `runSnapshotOrchestration`. Both methods sit in `snapshot.go`. Mirrors
  the syncer pattern of `Start` → `periodicUploader` (engine/syncer.go:580).
- **`failSnap` private helper:** rather than inline `if err := r.store.
  UpdateSnapshotState(...); err != nil { logger.Error(...) }` at six
  failure sites, the helper eliminates the duplication and centralizes
  the "use context.Background" decision in one place.
- **`snap.ID` and `snap.ShareName` reconstructed in the goroutine
  rather than re-fetched** for path derivation. The model methods
  `SnapshotDir / ManifestPath / MetadataDumpPath` only need those two
  fields. Saves a DB round-trip per orchestration; the state CRUD calls
  later in the pipeline operate by (share, id) anyway.

## Deviations from PATTERNS.md

None functional. The `runSnapshotOrchestration` goroutine body mirrors
the `periodicUploader` shape exactly (deferred cleanup before exit, no
panic recovery — Go orthodoxy is to let unexpected panics bubble; the
defer-on-doneCh is structured cleanup, not panic safety).

## TDD Gate Compliance

Task 2 (declared `tdd="true"`) followed RED → GREEN cadence:

- RED commit: `bf8d7e55 test(23-04): failing tests for WriteMetadataDumpAtomic + ValidateRetryTarget`
  — confirmed `undefined: snapshot.WriteMetadataDumpAtomic` /
  `snapshot.ValidateRetryTarget` build failure before GREEN.
- GREEN commit: `c602b849 feat(23-04): WriteMetadataDumpAtomic +
  ValidateRetryTarget + BlockStore.RemoteStore`
  — tests PASS under `-race -count=1`.

Task 1 and Task 3 were not TDD-declared (no `tdd="true"`); their
verification lives in plan 23-06 per the plan's own `<done>` block
("Integration test lives in plan 23-06.").

## Threat Mitigations Verified

| Threat | Status |
|--------|--------|
| T-23-04-RACE (goroutine outlives RemoveShare) | Foundation laid — centralized registry exists; cancel+Wait wiring lands in plan 23-05 |
| T-23-04-LEAK (orphan goroutine on Shutdown) | Foundation laid — `runtimeCancel` exists on Runtime; Shutdown wiring lands in plan 23-05 |
| T-23-04-INV (state=ready+RemoteDurable=true without verify) | Mitigated — D-23-03 enforced: ready+true only after VerifyRemoteDurability returns nil; NoSyncGate path explicitly sets RemoteDurable=false |
| T-23-04-CONCURRENT (two CreateSnapshot races past partial index) | Mitigated — D-23-01 enforced: state=creating row inserted BEFORE any I/O; Phase 22 D-08 partial unique index handles the rejection |
| T-23-04-SC (supply chain) | n/a — no new packages |

## Self-Check: PASSED

- `pkg/controlplane/runtime/snapshot.go` — FOUND
- `pkg/snapshot/dump.go` — FOUND
- `pkg/snapshot/dump_test.go` — FOUND
- `pkg/snapshot/retry.go` — FOUND
- `pkg/snapshot/retry_test.go` — FOUND
- Modifications to `pkg/controlplane/runtime/runtime.go`, `pkg/controlplane/store/{interface,snapshots}.go`, `pkg/blockstore/engine/engine.go` — present
- Commits `ab28d748`, `bf8d7e55`, `c602b849`, `e2de85fa` — present in `git log`
