---
phase: 22-snapshot-records-hash-manifest-gc-hold
verified: 2026-05-28T08:15:00Z
status: passed
score: 5/5
overrides_applied: 0
re_verification: false
---

# Phase 22: Snapshot Records + Hash Manifest + GC Hold — Verification Report

**Phase Goal:** Build snapshot persistence, hash manifest I/O, and GC hold integration.
**Verified:** 2026-05-28T08:15:00Z
**Status:** PASSED
**Re-verification:** No — initial verification

---

## Goal Achievement

### Observable Truths (ROADMAP Success Criteria)

| # | Truth | Status | Evidence |
|---|-------|--------|----------|
| 1 | Snapshot GORM model persists and queries correctly | VERIFIED | `pkg/controlplane/models/snapshot.go:28-60`, `pkg/controlplane/store/snapshots.go:1-133`; 9 SQLite integration tests pass under `-race` |
| 2 | Hash manifest writes sorted hex hashes and streams them back identically | VERIFIED | `pkg/snapshot/manifest.go`; 11 round-trip + atomicity + 100k-set tests pass under `-race` |
| 3 | GC mark phase includes snapshot-held hashes in live set (SnapshotHoldProvider) | VERIFIED | `pkg/blockstore/engine/gc.go:198-216` (interface), `gc.go:391-405` (wiring); `pkg/controlplane/runtime/snapshot_hold.go`; `blockgc.go:65,145` (injection) |
| 4 | Integration test: create snapshot → run GC → verify held blocks survive → delete snapshot → run GC → verify blocks collected | VERIFIED | `pkg/controlplane/runtime/snapshot_lifecycle_test.go:133-279`; all 3 sub-tests PASS under `-race` (`TestSnapshotLifecycleVsGC/snapshot_ready_preserves_held_block`, `.../snapshot_deletion_releases_held_block`, `.../RemoveShare_cleans_snapshots_tree`) |
| 5 | Control plane store CRUD works for snapshot lifecycle | VERIFIED | `pkg/controlplane/store/snapshots.go` (Create/Get/List/Delete/UpdateState); `pkg/controlplane/store/interface.go:449-583` (SnapshotStore interface composed into Store) |

**Score: 5/5 truths verified**

---

### Requirements Coverage (SNAP-01 through SNAP-05)

| Requirement | Description | Status | Evidence |
|-------------|-------------|--------|----------|
| SNAP-01 | Snapshot GORM model with ID (UUID), ShareName, State, MetadataEngine, ManifestCount, RemoteDurable, timestamps | VERIFIED | `models/snapshot.go:28-37` — all 8 D-11 fields present with correct GORM tags; UUID PK via `gorm:"primaryKey;size:36"`; partial unique index `idx_share_creating` on ShareName where state='creating'; autoCreateTime/autoUpdateTime; StateCreating/StateReady/StateFailed constants exported |
| SNAP-02 | On-disk layout `<share-data-dir>/snapshots/<id>/metadata.dump` + `manifest.hashes` (sorted hex ContentHash, one per line) | VERIFIED | `models/snapshot.go:44-59` (SnapshotDir/ManifestPath/MetadataDumpPath helpers); `pkg/snapshot/manifest.go` (sorted-ascending, LF-terminated, 64-hex per line); `WriteManifestAtomic` atomic temp+fsync+rename |
| SNAP-03 | SnapshotHoldProvider interface extends GC mark phase — active snapshot manifests inject hashes into live set | VERIFIED | `engine/gc.go:215-216` (HoldProvider interface); `engine/gc.go:391-405` (markPhase wiring, after EnumerateFileBlocks, before FlushAdd); `runtime/snapshot_hold.go` (SnapshotHoldProvider impl, filters state='ready', streams via streamManifest, fail-closed on missing manifest); `blockgc.go:65,145` (wired into both RunBlockGC and RunBlockGCForShare) |
| SNAP-04 | GC correctly skips blocks referenced by active snapshots; deleting a snapshot releases the GC hold | VERIFIED | `snapshot_lifecycle_test.go:137-198` (GC pass 1: hOrphan swept, hSnap survives; ObjectsSwept==1); `snapshot_lifecycle_test.go:201-237` (GC pass 2: hSnap swept after row+dir delete; ObjectsSwept==1); live run confirmed under `-race` |
| SNAP-05 | Control plane store CRUD for snapshot records (Create, List, Get, Delete) | VERIFIED | `store/snapshots.go:19-133` (5 methods on GORMStore); `store/interface.go:449-478` (SnapshotStore sub-interface, 5 declared methods including UpdateSnapshotState); composed into top-level Store at line 583; `models/models.go:11` (`&Snapshot{}` in AllModels for AutoMigrate); idx_share_creating fallback in `gorm.go:336` |

---

### Required Artifacts

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `pkg/controlplane/models/snapshot.go` | Snapshot struct + state constants + path helpers | VERIFIED | 61 lines; all D-11 fields, 3 path helpers, TableName(), GORM partial-index tag present |
| `pkg/controlplane/store/snapshots.go` | SnapshotStore GORM impl | VERIFIED | 133 lines; 5 substantive method bodies; state machine validated in validateStateTransition |
| `pkg/snapshot/manifest.go` | WriteManifest / WriteManifestAtomic / ReadManifest | VERIFIED | 124 lines; all 3 public functions implemented; atomic write via writeAndSync helper; bufio.Scanner with 1MiB cap |
| `pkg/blockstore/engine/gc.go` (modified) | HoldProvider interface + Options field + markPhase wiring | VERIFIED | HoldProvider interface at line 215; Options.HoldProvider field at line 179; markPhase invokes HeldHashes after EnumerateFileBlocks, before FlushAdd (lines 391-405) |
| `pkg/controlplane/runtime/snapshot_hold.go` | SnapshotHoldProvider + snapshotHoldForRemote builder | VERIFIED | 123 lines; SnapshotHoldProvider.HeldHashes streams ready-snapshot manifests; streamManifest helper closes file handle inside; nil-safe |
| `pkg/controlplane/runtime/blockgc.go` (modified) | HoldProvider injected in RunBlockGC + RunBlockGCForShare | VERIFIED | `opts.HoldProvider = r.snapshotHoldForRemote(...)` appears at lines 65 and 145 (2 occurrences, one per GC entry point) |
| `pkg/controlplane/runtime/shares/service.go` (modified) | RemoveShare snapshots/ cleanup hook | VERIFIED | Lines 779-784; `os.RemoveAll(filepath.Join(localStoreDir, "snapshots"))` runs after mutex release, before block-store close; logs Warn on failure, does not abort |

---

### Key Link Verification

| From | To | Via | Status | Details |
|------|----|-----|--------|---------|
| `blockgc.go:RunBlockGC` | `engine.CollectGarbage` | `opts.HoldProvider = r.snapshotHoldForRemote(...)` | WIRED | Line 65 assigns provider before calling collectGarbageFn |
| `blockgc.go:RunBlockGCForShare` | `engine.CollectGarbage` | `opts.HoldProvider = r.snapshotHoldForRemote(...)` | WIRED | Line 145; second entry point also covered |
| `engine.CollectGarbage` | `markPhase` | `options.HoldProvider` threaded as parameter | WIRED | `gc.go:313` passes `options.HoldProvider` to markPhase |
| `markPhase` | `HoldProvider.HeldHashes` | callback same shape as EnumerateFileBlocks | WIRED | `gc.go:402` calls `hold.HeldHashes(ctx, remoteEndpointID, shares, cb)` after per-share loops, before FlushAdd |
| `SnapshotHoldProvider.HeldHashes` | `snapshot.ReadManifest` | `streamManifest(manifestPath, fn)` | WIRED | `snapshot_hold.go:75,96-110`; opens file, calls ReadManifest, iterates via HashSet.ForEach |
| `SnapshotHoldProvider.HeldHashes` | `store.ListSnapshots` | `p.rt.store.ListSnapshots(ctx, shareName)` | WIRED | `snapshot_hold.go:65`; filters to StateReady at line 72 |
| `store/gorm.go` | `models.Snapshot` table | `AllModels()` + AutoMigrate | WIRED | `models/models.go:11`; `gorm.go:336` partial-index fallback |
| `RemoveShare` | `snapshots/` directory | `os.RemoveAll(filepath.Join(localStoreDir, "snapshots"))` | WIRED | `shares/service.go:781`; runs after registry delete, before block-store close |

---

### Data-Flow Trace (Level 4)

| Artifact | Data Variable | Source | Produces Real Data | Status |
|----------|---------------|--------|-------------------|--------|
| `SnapshotHoldProvider.HeldHashes` | `snaps` from `ListSnapshots` | GORM DB query (`WHERE share_name = ? ORDER BY created_at DESC`) | Yes — real DB rows | FLOWING |
| `streamManifest` | `hs` from `ReadManifest` | `os.Open` + `bufio.Scanner` reads from on-disk `manifest.hashes` file | Yes — real file I/O | FLOWING |
| `markPhase` (hold path) | `HashesMarked` incremented per hash | `HeldHashes` callback → `gcs.Add(h)` writes to Badger-backed GCState live set | Yes — disk-backed live set | FLOWING |
| `sweepPhase` | deletes via `remoteStore.Walk` | Consults `gcs.Has()` against the populated live set | Yes — real remote object deletions | FLOWING |

---

### Behavioral Spot-Checks

| Behavior | Command | Result | Status |
|----------|---------|--------|--------|
| Integration test: held block survives GC | `go test ./pkg/controlplane/runtime/... -run TestSnapshotLifecycleVsGC -count=1 -race` | PASS (3/3 sub-tests, 0.19s) | PASS |
| GC engine HoldProvider unit tests | `go test ./pkg/blockstore/engine/... -run TestGCMarkSweep -count=1 -race` | PASS (8.12s) | PASS |
| Manifest round-trip + atomicity | `go test ./pkg/snapshot/... -count=1 -race` | PASS (11 tests, 1.62s) | PASS |
| Model field-set guard + path helpers | `go test ./pkg/controlplane/models/... -run TestSnapshot_ -count=1 -race` | PASS (4 tests, 1.22s) | PASS |
| Store CRUD + idx_share_creating integration | `go test -tags=integration ./pkg/controlplane/store/... -run TestSnapshot_ -count=1 -race` | PASS (9 tests, 2.70s) | PASS |
| SnapshotHoldProvider unit tests | `go test ./pkg/controlplane/runtime/... -run TestSnapshotHoldProvider -count=1 -race` | PASS (5 tests) | PASS |
| Full package suite, no regressions | `go test ./pkg/...` | All 30 packages PASS, 0 FAIL | PASS |

---

### Anti-Patterns Found

| File | Line | Pattern | Severity | Impact |
|------|------|---------|----------|--------|
| (none) | — | — | — | No TBD/FIXME/XXX/placeholder patterns found in any Phase 22 file |

`BackupHoldProvider` legacy symbol confirmed absent from `gc.go` and `gc_test.go` — deliberately deleted per plan (no compat shim, per project invariant).

---

### Probe Execution

Step 7c: SKIPPED — no `scripts/*/tests/probe-*.sh` files declared in PLAN.md or SUMMARY.md for Phase 22.

---

### Human Verification Required

None — all observable truths are programmatically verifiable via test execution and static code inspection. The implementation is backend-only (no UI, no visual rendering, no external service integration). All checks passed.

---

## Gaps Summary

No gaps. All five SNAP requirements are fully implemented and wired end-to-end. The integration test `TestSnapshotLifecycleVsGC` exercises the complete path from snapshot creation through GC hold to hold release, including the `RemoveShare` cleanup hook, and passes under the Go race detector.

**One architectural note (non-blocking, by design):** The per-snapshot delete path in `TestSnapshotLifecycleVsGC/snapshot_deletion_releases_held_block` manually performs both the DB row deletion and the `os.RemoveAll` of the snapshot directory. The CONTEXT explicitly scoped the per-snapshot filesystem cleanup to Phase 23 orchestration (D-07 states "Phase 23 orchestration should hold a brief lock around Delete"). This is not a gap — it is a deliberate deferral tracked in the CONTEXT deferred section.

---

_Verified: 2026-05-28T08:15:00Z_
_Verifier: gsd-verifier_
