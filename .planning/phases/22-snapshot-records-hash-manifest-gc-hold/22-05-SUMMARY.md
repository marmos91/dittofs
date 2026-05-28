---
phase: 22-snapshot-records-hash-manifest-gc-hold
plan: 05
subsystem: infra
tags: [snapshot, gc, hold-provider, runtime, blockstore, gorm]

requires:
  - phase: 22-snapshot-records-hash-manifest-gc-hold/22-01
    provides: Snapshot model + state constants (creating/ready/failed) + ManifestPath helper
  - phase: 22-snapshot-records-hash-manifest-gc-hold/22-02
    provides: ReadManifest / WriteManifestAtomic on-disk hash manifest format
  - phase: 22-snapshot-records-hash-manifest-gc-hold/22-03
    provides: SnapshotStore (CreateSnapshot, ListSnapshots, UpdateSnapshotState) on the GORM control-plane store
  - phase: 22-snapshot-records-hash-manifest-gc-hold/22-04
    provides: engine.HoldProvider interface + engine.Options.HoldProvider injection point in CollectGarbage mark phase
provides:
  - SnapshotHoldProvider streaming ready-snapshot manifest hashes into the block-GC mark phase
  - Runtime.snapshotHoldForRemote builder wired into both RunBlockGC and RunBlockGCForShare per-remote loops
  - Service.RemoveShare snapshot-directory cleanup hook before block-store close
affects: [block-gc, snapshot-lifecycle, share-removal]

tech-stack:
  added: []
  patterns:
    - "Per-remote scoped HoldProvider constructed at GC entry: closure-captured share list is the source of truth; engine-passed shares are informational"
    - "Streaming I/O over manifest file via ReadManifest -> HashSet.ForEach -> engine callback; file handle closed inside helper (no defer-in-loop)"
    - "Fail-closed on missing manifest for ready row: orphan-not-deleted preferred over live-data-deleted"
    - "Cascade-delete with FS hook: os.RemoveAll(<localStoreDir>/snapshots) inside RemoveShare alongside registry removal, before block-store close; failure warns but does not abort"

key-files:
  created:
    - pkg/controlplane/runtime/snapshot_hold.go
    - pkg/controlplane/runtime/snapshot_hold_test.go
  modified:
    - pkg/controlplane/runtime/blockgc.go
    - pkg/controlplane/runtime/shares/service.go

key-decisions:
  - "Closure-captured per-remote share list is the source of truth; engine-passed shares arg is ignored (per-remote scoping established at construction)"
  - "configID is captured for log correlation only and not used for filtering — the per-remote share list already encodes the remote boundary by construction"
  - "Manifest read uses a streamManifest helper so defer f.Close() lives inside the helper rather than accumulating in the outer share/snapshot loop"
  - "Empty localStoreDir (memory backend) is skipped silently — no on-disk manifest can exist; no error surfaces"
  - "RemoveShare cleanup runs BEFORE block-store close so snapshots/ removal cannot race with any block-store finalizers; failure is logged at Warn and does not abort registry removal"
  - "Tests respect the idx_share_creating partial-unique index by transitioning each row out of 'creating' before inserting the next snapshot for the same share"

patterns-established:
  - "engine.HoldProvider injection point pattern: runtime owns construction, engine owns invocation, callback signature mirrors EnumerateFileBlocks"
  - "Per-share cascade-delete on RemoveShare: capture all per-share paths under mutex, perform FS removal after mutex release, log-and-continue on failure"

requirements-completed: [SNAP-03]

duration: 38min
completed: 2026-05-28
---

# Phase 22 Plan 05: Snapshot Hold Provider Wiring Summary

**SnapshotHoldProvider streams ready-snapshot manifest hashes into the block-GC mark phase, wired into both RunBlockGC paths and paired with a snapshots/ cleanup hook in RemoveShare to close SNAP-03 end-to-end.**

## Performance

- **Duration:** ~38 min
- **Started:** 2026-05-28T07:12:00Z (approx, plan execution kickoff)
- **Completed:** 2026-05-28T07:50:20Z
- **Tasks:** 3
- **Files modified:** 4 (2 created, 2 modified)

## Accomplishments

- `SnapshotHoldProvider` implements `engine.HoldProvider`, scoped per-remote at construction; streams the union of every `state='ready'` snapshot's manifest hashes through the engine mark-phase callback
- `Runtime.RunBlockGC` and `Runtime.RunBlockGCForShare` now attach the provider to `engine.Options.HoldProvider` on every per-remote invocation
- `Service.RemoveShare` captures `localStoreDir` under the registry mutex and removes `<localStoreDir>/snapshots/` best-effort before closing the block store; failure logs at Warn and does not abort
- Five unit tests pin the contract under `-race`: nil-store no-op, ready-state filter, fail-closed on missing manifest, memory-share skip, multi-share union

## Task Commits

1. **Task 1: SnapshotHoldProvider type + Runtime.snapshotHoldForRemote builder** — `25c311c6` (feat)
2. **Task 2: Wire RunBlockGC + RunBlockGCForShare + add Service.RemoveShare cleanup hook** — `088181f8` (feat)
3. **Task 3: SnapshotHoldProvider unit tests** — `41cf5cd5` (test)

## Files Created/Modified

- `pkg/controlplane/runtime/snapshot_hold.go` — `SnapshotHoldProvider` type, `HeldHashes` implementation, `streamManifest` helper, `Runtime.snapshotHoldForRemote` builder
- `pkg/controlplane/runtime/snapshot_hold_test.go` — 5 unit tests against in-memory SQLite GORMStore
- `pkg/controlplane/runtime/blockgc.go` — `opts.HoldProvider = r.snapshotHoldForRemote(entry.ConfigID, entry.Shares)` inserted in both `RunBlockGC` and `RunBlockGCForShare` per-remote loops
- `pkg/controlplane/runtime/shares/service.go` — `RemoveShare` captures `localStoreDir` under mutex, calls `os.RemoveAll(filepath.Join(localStoreDir, "snapshots"))` after unlock and before block-store close; logs Warn on failure

## Decisions Made

- **Engine-passed `shares` argument is informational only.** The `HeldHashes` implementation iterates over the closure-captured `p.shares` from `snapshotHoldForRemote`. The two arguments are co-derived from the same per-remote scope at the same call site, so they are effectively redundant — but ownership of "what counts as held for this remote" lives at construction time, not invocation time. This honours the per-remote scoping decision (D-03) literally and keeps the engine free of any snapshot-aware logic.
- **`configID` not used for filtering.** Each share in the captured list, by construction, points at the same remote-store config — so further filtering at provider time would be redundant. `configID` is captured purely for log correlation.
- **Manifest streaming via helper.** `streamManifest(path, fn)` owns the `os.Open` → `defer f.Close()` → `ReadManifest` → `HashSet.ForEach` chain, so the caller loop does not accumulate deferred closes across many snapshots.
- **Empty `localStoreDir` skipped silently.** Memory backends have no on-disk manifest; surfacing this as an error would force every caller to special-case the path. The skip is the same shape `LocalStoreDir`'s callers already handle for migration journals.
- **Fail-closed semantics on missing manifest.** The atomic-write contract from plan 22-02 guarantees a `ready` row implies a complete manifest on disk. Absence is corruption, not normalcy — surfacing an `os.ErrNotExist`-wrapped error aborts the whole GC run via `markPhase`'s existing fail-closed path. INV-04 (mark fail-closed) is preserved by exactly the engine machinery that already exists; the provider only has to return a non-nil error.
- **Cleanup ordering in `RemoveShare`.** The hook runs AFTER the mutex release (so a slow `os.RemoveAll` does not block the registry) and BEFORE the block-store close (so the block store's own finalizers can never observe a half-deleted snapshots tree). A removal failure logs Warn and proceeds — orphaned files are operationally harmless; the DB row is the source of truth.

## Deviations from Plan

None — plan executed exactly as written.

## Issues Encountered

- **Snapshot-row insertion conflict in `TestSnapshotHoldProvider_FiltersByReadyState`.** First pass tried to insert three snapshots for the same share in the order creating → ready → failed; the second `CreateSnapshot` call (state=creating intermediate before the transition to ready) collided with the `idx_share_creating` partial-unique index on the still-creating first row. Fixed by re-ordering: insert+transition the ready row first, then insert+transition the failed row, then leave one row in state=creating. Documented inline in the test so future readers see the constraint immediately. No production code change.

## User Setup Required

None — no external service configuration required.

## Next Phase Readiness

- SNAP-03 closed end-to-end: snapshot records → manifest reader → SnapshotStore CRUD → engine HoldProvider injection point → runtime provider wired into both GC paths.
- Plan 22-06 will exercise the wire end-to-end via a lifecycle integration test (write blocks → snapshot → GC → assert held; remove share → assert snapshots/ directory gone).
- No new third-party dependencies; everything already in `go.mod`.

## Threat Flags

None — no new network endpoints, auth surface, or trust boundaries introduced. The provider reads disk paths derived from `LocalStoreDir`, not user input; `os.RemoveAll` runs on a known per-share directory, not a user-controlled string.

## Self-Check: PASSED

- FOUND: `pkg/controlplane/runtime/snapshot_hold.go`
- FOUND: `pkg/controlplane/runtime/snapshot_hold_test.go`
- FOUND: commits `25c311c6`, `088181f8`, `41cf5cd5`
- `go build ./pkg/controlplane/...` exits 0
- `go vet ./pkg/controlplane/...` exits 0
- `go test ./pkg/controlplane/runtime/... -count=1 -race` exits 0
- `grep -c 'opts.HoldProvider = r.snapshotHoldForRemote' pkg/controlplane/runtime/blockgc.go` returns 2
- No GSD metadata in `pkg/controlplane/runtime/snapshot_hold.go` (or its test file, or new diff hunks in `blockgc.go` / `shares/service.go`)

---
*Phase: 22-snapshot-records-hash-manifest-gc-hold*
*Plan: 05*
*Completed: 2026-05-28*
