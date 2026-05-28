---
phase: 23-snapshot-create-orchestration-sync-gate
plan: 03
subsystem: controlplane/runtime
tags: [snapshot, hold, gc, mutex, tdd]
requires:
  - 22-snapshot-records-hash-manifest-gc-hold/22-SUMMARY.md
provides:
  - "SnapshotHoldProvider filter by manifest-on-disk (D-23-02)"
  - "Provider-level sync.RWMutex + AcquireDeleteLock helper (D-23-04)"
affects:
  - pkg/controlplane/runtime/snapshot_hold.go
  - pkg/controlplane/runtime/snapshot_hold_test.go
  - pkg/controlplane/runtime/snapshot_lifecycle_test.go
tech_stack:
  added: []
  patterns:
    - "Provider-wide sync.RWMutex (mirrors pkg/blockstore/engine/syncer.go field-placement convention)"
    - "AcquireDeleteLock returns release closure (mirrors std lib `sync.Mutex` unlock pattern)"
key_files:
  created: []
  modified:
    - pkg/controlplane/runtime/snapshot_hold.go
    - pkg/controlplane/runtime/snapshot_hold_test.go
    - pkg/controlplane/runtime/snapshot_lifecycle_test.go
decisions:
  - "Filter strategy: FS-walk via os.Stat(snap.ManifestPath(localStoreDir)) — no DB schema change"
  - "Lock granularity: provider-level single sync.RWMutex (per-snapshot upgrade deferred until head-of-line blocking surfaces)"
  - "Race regression uses real on-disk SQLite (not :memory:) so the connection pool shares schema across reader goroutines"
metrics:
  duration_minutes: 12
  completed: 2026-05-28
  tasks_total: 2
  tasks_completed: 2
  files_modified: 3
  files_created: 0
---

# Phase 23 Plan 03: SnapshotHoldProvider Revision Summary

Revised the Phase 22 `SnapshotHoldProvider` so the block-GC hold filter is "any snapshot whose `manifest.hashes` exists on disk, regardless of state" (D-23-02). Added a provider-level `sync.RWMutex` and an exported `AcquireDeleteLock` helper for the upcoming orchestration delete path (D-23-04), resolving the Phase 22 deferred delete-vs-`HeldHashes` race.

## What Changed

### `pkg/controlplane/runtime/snapshot_hold.go`

1. **Filter swap (D-23-02).** Removed the `if snap.State != models.StateReady { continue }` gate. Replaced with `os.Stat(snap.ManifestPath(...))`: `os.IsNotExist` → continue (no hold), any other error → wrapped + propagated (INV-04 fail-closed). The `models` import was dropped (no longer referenced); `io/fs` added for `fs.ErrNotExist`.
2. **RWMutex (D-23-04).** Added `mu sync.RWMutex` field. `HeldHashes` takes `p.mu.RLock(); defer p.mu.RUnlock()` immediately after the nil-safety guards, before the per-share loop. New exported `AcquireDeleteLock() (release func())` takes `p.mu.Lock()` and returns `p.mu.Unlock` — to be used by the orchestration delete path in plans 23-04 / 23-05.
3. Doc comment on `SnapshotHoldProvider` rewritten to describe the new filter semantics and the lock contract.
4. Added `state` key to the per-snapshot `slog.Debug` line so operators can see which lifecycle state contributed (now informative since multiple states qualify).

### `pkg/controlplane/runtime/snapshot_hold_test.go`

- Replaced `TestSnapshotHoldProvider_FiltersByReadyState` with table-driven `TestSnapshotHoldProvider_FilterByManifestOnDisk` covering all 6 plan-enumerated rows: {ready, creating, failed} × {manifest present, manifest absent}.
- Replaced `TestSnapshotHoldProvider_FailClosed_OnMissingManifest` (semantics inverted by D-23-02) with `TestSnapshotHoldProvider_FailClosed_OnManifestStatError`. Uses chmod 0o000 on the snapshot dir to elicit EACCES from `os.Stat` and asserts the error propagates (no `os.IsNotExist` short-circuit). Auto-skips when running as root.
- Existing `TestSnapshotHoldProvider_NilStore_NoOp`, `TestSnapshotHoldProvider_MemoryShare_Skipped`, `TestSnapshotHoldProvider_MultipleSharesUnion` unchanged — still valid under new filter.

### `pkg/controlplane/runtime/snapshot_lifecycle_test.go`

- Added `TestSnapshotHoldProvider_DeleteVsHeldHashes_Race`: 4 reader goroutines × 50 iterations of `HeldHashes` + 1 writer goroutine × 50 iterations of `AcquireDeleteLock` + 50µs sleep + release. Asserts:
  - No panics (`recover()` + `atomic.Int64` counter).
  - No torn reads (every observation = full expected hold set of 12 hashes).
  - No deadlock (15 s `time.After` watchdog).
- Required `path/filepath`, `sync`, `sync/atomic`, `runtime/shares` imports.

## TDD Cadence (per-task RED → GREEN)

Each task followed strict TDD with separate failing-test and implementation commits:

| Task | RED commit (test fails) | GREEN commit (test passes) |
| ---- | ----------------------- | -------------------------- |
| 1    | `8399ef81 test(23-03): add failing tests for manifest-on-disk hold filter` | `40cbca6c refactor(23-03): filter snapshot hold by manifest-on-disk (D-23-02)` |
| 2    | `5929ead2 test(23-03): add failing race regression for delete-vs-HeldHashes` | `043841a3 feat(23-03): add provider-level RWMutex + AcquireDeleteLock (D-23-04)` |

RED phase for Task 1 failed on the three new behaviors (creating + manifest, failed + manifest, ready + no manifest — the existing `state==Ready` filter mis-classified all three). RED phase for Task 2 failed at build time on the missing `AcquireDeleteLock` symbol.

## Verification

| Check | Result |
| ----- | ------ |
| `go test ./pkg/controlplane/runtime/... -race -count=1` | PASS |
| `go test ./pkg/controlplane/runtime/... -run "TestSnapshotHoldProvider_DeleteVsHeldHashes_Race\|TestSnapshotLifecycleVsGC" -race -count=5` | PASS (5/5 iterations) |
| `go build ./...` | clean |
| `go vet ./pkg/controlplane/runtime/...` | clean |
| `gofmt -s -l pkg/controlplane/runtime/...` | clean |
| `grep -nv '^[[:space:]]*//' pkg/controlplane/runtime/snapshot_hold.go \| grep -c "snap.State != models.StateReady"` | `0` (old filter gone) |
| `grep -n "mu .*sync.RWMutex" pkg/controlplane/runtime/snapshot_hold.go` | line 42 |
| `grep -n "p.mu.RLock\|p.mu.RUnlock" pkg/controlplane/runtime/snapshot_hold.go` | lines 56–57 |
| `grep -n "func.*AcquireDeleteLock" pkg/controlplane/runtime/snapshot_hold.go` | line 131 |
| `grep -n "ManifestPath\|os.Stat" pkg/controlplane/runtime/snapshot_hold.go` | lines 79–80 |

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] `:memory:` SQLite cannot survive multi-goroutine pool fan-out**

- **Found during:** Task 2 GREEN
- **Issue:** Initial race test reused the `cpstore.New(:memory:)` pattern from `snapshot_hold_test.go`, but reader goroutines intermittently hit `no such table: snapshots`. SQLite `:memory:` DBs are per-connection, so when the GORM/sql pool spins up a second connection under contention, it gets a brand-new empty DB.
- **Fix:** Switched the race test to a real on-disk SQLite path under `t.TempDir()`. All connections in the pool now see the same migrated schema. Note in test comment explains the rationale.
- **Files modified:** `pkg/controlplane/runtime/snapshot_lifecycle_test.go`
- **Commit:** rolled into `043841a3` (GREEN for Task 2)

**2. [Rule 1 - Bug] WaitGroup `Add` after `Wait` race-detector flag**

- **Found during:** Task 2 GREEN
- **Issue:** First draft of race test spawned a `wg.Wait()` watcher goroutine before the per-reader `wg.Add(1)` calls. Race detector correctly flagged this as `Add`-after-`Wait` (testing-internal field race surfaced via tRunner).
- **Fix:** Reserved all WaitGroup slots up front (`wg.Add(readers + 1)`) before spawning either the watcher or the workers. Comment in test explains the constraint.
- **Files modified:** `pkg/controlplane/runtime/snapshot_lifecycle_test.go`
- **Commit:** rolled into `043841a3`

### Architectural Changes

None.

## Plan Choices Made (planner discretion per CONTEXT)

- **Filter strategy (D-23-02):** FS-walk via `os.Stat`. No DB schema change; the manifest is the ground truth (Phase 22 D-04) so stating it directly is the most honest implementation. The alternative (DB-driven `WHERE manifest_exists`) would have required a new column or join and offered no behavioral advantage.
- **Lock granularity (D-23-04):** Provider-level single `sync.RWMutex`. Simpler than `sync.Map[snapID]*RWMutex`; per-CONTEXT acceptable for typical snapshot counts (≤ low hundreds per share). Per-ID upgrade is tracked under Phase 23 deferred ideas if head-of-line blocking surfaces in practice.

## TDD Gate Compliance

- Task 1: RED commit `8399ef81` (test only) precedes GREEN commit `40cbca6c` (implementation). Compliant.
- Task 2: RED commit `5929ead2` (test only) precedes GREEN commit `043841a3` (implementation). Compliant.
- No REFACTOR commits needed; both implementations were minimal and clean as written.

## Threat Mitigations Verified

| Threat | Status |
| ------ | ------ |
| T-23-03-RACE (delete-vs-HeldHashes) | Mitigated — `sync.RWMutex` field + `AcquireDeleteLock`; race regression PASS at -count=5 under -race. |
| T-23-03-FAIL-CLOSED (manifest I/O error during mark) | Mitigated — non-IsNotExist errors from `os.Stat` propagate; covered by `TestSnapshotHoldProvider_FailClosed_OnManifestStatError`. |

## Self-Check: PASSED

- `pkg/controlplane/runtime/snapshot_hold.go` — modified, present
- `pkg/controlplane/runtime/snapshot_hold_test.go` — modified, present
- `pkg/controlplane/runtime/snapshot_lifecycle_test.go` — modified, present
- Commits `8399ef81`, `40cbca6c`, `5929ead2`, `043841a3` — present in `git log`
