---
phase: 21-per-engine-backup-drivers
plan: 05
subsystem: metadata-backup
tags: [race-safety, allocation-bounds, signal-ordering, memory, badger, postgres, gap-closure]
dependency_graph:
  requires: [crc-before-commit-badger, crc-before-commit-postgres]
  provides: [race-safe-memory-backup, bounded-restore-allocations, correct-signal-ordering]
  affects: [pkg/metadata/store/memory/backup.go, pkg/metadata/store/badger/backup.go, pkg/metadata/store/postgres/backup.go]
tech_stack:
  added: []
  patterns: [lock-copy-unlock, allocation-cap, signal-after-snapshot]
key_files:
  created: []
  modified:
    - pkg/metadata/store/memory/backup.go
    - pkg/metadata/store/badger/backup.go
    - pkg/metadata/store/postgres/backup.go
decisions:
  - Memory Backup acquires rollupMu.RLock and syncedMu.RLock with shallow-copy to avoid aliasing live maps
  - Badger Backup creates envelope writer inside db.View callback so first Write fires after MVCC snapshot
  - maxRestoreAllocSize (256 MiB) caps untrusted key/value sizes in Badger restore
  - maxRestorePayloadSize (256 MiB) caps untrusted gob payload size in Memory restore
  - Postgres restoreTable rejects dataLen > math.MaxInt64 before int64 cast
metrics:
  duration: 185s
  completed: "2026-05-27T14:22:53Z"
  tasks_completed: 3
  tasks_total: 3
  files_modified: 3
---

# Phase 21 Plan 05: Memory Race, Signal Ordering, and Allocation Bounds Summary

Race-safe memory backup under dedicated rollupMu/syncedMu, Badger signal-after-snapshot ordering, and bounded restore allocations across all three drivers.

## What Changed

### Task 1: Memory -- acquire rollupMu and syncedMu during Backup snapshot (e849d847)

The `Backup` method read `s.rollupOffsets` and `s.synced` directly inside the struct literal under only `s.mu.RLock`, but those maps are governed by their own mutexes (`rollupMu` and `syncedMu`). Concurrent `SetRollupOffset` or `MarkSynced` calls could produce torn reads.

**Fix:** Removed `RollupOffsets` and `Synced` from the struct literal. After the literal, acquire `s.rollupMu.RLock`, shallow-copy `s.rollupOffsets` into a fresh map, release `s.rollupMu.RUnlock`. Same pattern for `s.syncedMu.RLock` and `s.synced`. Assign the copies to `snap.RollupOffsets` and `snap.Synced`. The copies avoid aliasing the live maps since the dedicated locks are released before gob encoding.

### Task 2: Badger -- move signal after db.View entry + add allocation bounds (011cb3ef)

**Signal ordering fix:** `backup.NewWriter(w, badgerEngineTag)` was called before `s.db.View()`, writing the envelope header to the output writer and triggering the `signalWriter` in the `ConcurrentWriter` test before the MVCC snapshot was established. Moved `backup.NewWriter` and the schema version write inside the `db.View` callback. The `envW` variable is declared before the callback and assigned inside; `envW.Finish()` remains outside since it runs after the callback returns.

**Allocation bounds:** Added `maxRestoreAllocSize = 256 << 20` (256 MiB) constant. Before `make([]byte, keyLen)` and `make([]byte, valLen)` in `Restore`, check if the size exceeds the cap. Oversized values return `ErrRestoreCorrupt` with the offending size.

### Task 3: Memory + Postgres -- allocation bounds for untrusted stream sizes (a97b4ce5)

**Memory:** Added `maxRestorePayloadSize = 256 << 20` constant. Before `make([]byte, payloadLen)`, check if `payloadLen > uint64(maxRestorePayloadSize)`. Rejects with `ErrRestoreCorrupt`.

**Postgres:** Added `math` import. Before `io.LimitReader(payloadR, int64(dataLen))` in `restoreTable`, reject `dataLen > uint64(math.MaxInt64)` with `ErrRestoreCorrupt`. A uint64 exceeding MaxInt64 would wrap to a negative int64, causing LimitReader to return EOF immediately and desynchronize the stream parser.

## Verification Results

| Check | Result |
|-------|--------|
| Memory conformance (5 subtests, race, count=5) | PASS |
| Badger conformance (5 subtests, count=1) | PASS |
| Postgres build (integration tags) | PASS |
| go vet (all three packages) | PASS |
| rollupMu.RLock in memory backup | Present |
| syncedMu.RLock in memory backup | Present |
| backup.NewWriter inside db.View | Present |
| maxRestoreAllocSize (Badger) | 256 MiB |
| maxRestorePayloadSize (Memory) | 256 MiB |
| dataLen > math.MaxInt64 guard (Postgres) | Present |

## Deviations from Plan

None -- plan executed exactly as written.

## Known Stubs

None.

## Threat Mitigations

| Threat ID | Status | Implementation |
|-----------|--------|----------------|
| T-21-05-01 | Mitigated | rollupMu.RLock + syncedMu.RLock with shallow copy in memory Backup |
| T-21-05-02 | Mitigated | maxRestorePayloadSize (256 MiB) cap in memory Restore |
| T-21-05-03 | Mitigated | maxRestoreAllocSize (256 MiB) cap on keyLen/valLen in Badger Restore |
| T-21-05-04 | Mitigated | dataLen > math.MaxInt64 guard in Postgres restoreTable |
| T-21-05-05 | Mitigated | backup.NewWriter moved inside db.View callback |

## Self-Check: PASSED

All 3 files found. All 3 commits found. All 5 content markers verified.
