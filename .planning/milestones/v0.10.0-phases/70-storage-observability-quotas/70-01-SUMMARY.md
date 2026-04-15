---
phase: 70-storage-observability-quotas
plan: 01
subsystem: metadata, blockstore, models
tags: [atomic-counter, quota, stats, observability, gorm]

# Dependency graph
requires: []
provides:
  - QuotaBytes int64 field on Share model (GORM auto-migrated)
  - Atomic usage counters (usedBytes) in memory, badger, postgres metadata stores
  - BlockStore.Stats() returns real UsedSize from LocalDiskUsed
  - GetUsedBytes() method on all metadata store implementations
  - O(1) GetFilesystemStatistics via atomic counter (replaces O(n) file scan)
affects: [70-02, 70-03]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Atomic counter pattern: sync/atomic.Int64 for O(1) usage tracking across all metadata stores"
    - "Delta tracking in transaction PutFile/DeleteFile for incremental counter updates"
    - "Startup initialization from full scan (badger) or SQL SUM (postgres)"

key-files:
  created:
    - pkg/blockstore/engine/stats_test.go
    - pkg/metadata/store/memory/counter_test.go
  modified:
    - pkg/controlplane/models/share.go
    - pkg/blockstore/engine/engine.go
    - pkg/metadata/store/memory/store.go
    - pkg/metadata/store/memory/server.go
    - pkg/metadata/store/memory/transaction.go
    - pkg/metadata/store/badger/store.go
    - pkg/metadata/store/badger/server.go
    - pkg/metadata/store/badger/transaction.go
    - pkg/metadata/store/postgres/store.go
    - pkg/metadata/store/postgres/server.go
    - pkg/metadata/store/postgres/transaction.go

key-decisions:
  - "Track only regular file sizes (directories, symlinks, devices excluded from counter)"
  - "Delta tracking at transaction layer (PutFile/DeleteFile) for consistency across all stores"
  - "Badger GetFilesystemStatistics still scans for file count but reads bytes from atomic counter"

patterns-established:
  - "Atomic counter pattern: Add usedBytes atomic.Int64 to store struct, track deltas in PutFile/DeleteFile"
  - "GetUsedBytes() method: O(1) exported accessor for the atomic counter"

requirements-completed: [STATS-01, STATS-03]

# Metrics
duration: 13min
completed: 2026-03-21
---

# Phase 70 Plan 01: Data Model Foundation Summary

**QuotaBytes field on Share model, atomic O(1) usage counters in all three metadata stores, and BlockStore.Stats() wired to real LocalDiskUsed**

## Performance

- **Duration:** 13 min
- **Started:** 2026-03-21T09:28:22Z
- **Completed:** 2026-03-21T09:41:12Z
- **Tasks:** 2
- **Files modified:** 13

## Accomplishments
- Added QuotaBytes int64 field to Share model with GORM auto-migration support (0 = unlimited)
- Fixed BlockStore.Stats() to return real UsedSize/AvailableSize/AverageSize from LocalDiskUsed instead of hardcoded 0
- Added atomic.Int64 usage counters to all three metadata store implementations (memory, badger, postgres)
- Counters are updated on every size-changing operation: create, update/write, truncate, and remove
- GetFilesystemStatistics now uses O(1) atomic read instead of O(n) file scan for UsedBytes
- Badger/postgres stores initialize the counter from full file scan / SQL SUM on startup

## Task Commits

Each task was committed atomically:

1. **Task 1: Share model QuotaBytes field + BlockStore.Stats() UsedSize** - `25e7a19d` (feat)
2. **Task 2: Atomic usage counters in all metadata store implementations** - `79b581c8` (feat)

## Files Created/Modified
- `pkg/controlplane/models/share.go` - Added QuotaBytes int64 field with GORM column:quota_bytes
- `pkg/blockstore/engine/engine.go` - Fixed Stats() to wire UsedSize to localStats.DiskUsed, compute AvailableSize and AverageSize
- `pkg/blockstore/engine/stats_test.go` - Tests for Stats() empty store, UsedSize wiring, AvailableSize, AverageSize
- `pkg/metadata/store/memory/store.go` - Added usedBytes atomic.Int64 field and GetUsedBytes() method
- `pkg/metadata/store/memory/server.go` - GetFilesystemStatistics and computeStatistics use atomic counter
- `pkg/metadata/store/memory/transaction.go` - PutFile/DeleteFile/DeleteShare track size deltas
- `pkg/metadata/store/memory/counter_test.go` - 7 tests for counter accuracy (create, update, truncate, remove, directory, stats match)
- `pkg/metadata/store/badger/store.go` - Added usedBytes field, GetUsedBytes(), initUsedBytesCounter()
- `pkg/metadata/store/badger/server.go` - GetFilesystemStatistics uses atomic counter for bytes
- `pkg/metadata/store/badger/transaction.go` - PutFile/DeleteFile track size deltas
- `pkg/metadata/store/postgres/store.go` - Added usedBytes field, GetUsedBytes(), initUsedBytesCounter()
- `pkg/metadata/store/postgres/server.go` - GetFilesystemStatistics uses atomic counter for bytes
- `pkg/metadata/store/postgres/transaction.go` - PutFile/DeleteFile track size deltas

## Decisions Made
- Track only regular file sizes (directories, symlinks, devices excluded) -- consistent with existing GetFilesystemStatistics behavior
- Delta tracking at transaction layer (PutFile/DeleteFile) ensures all code paths that modify files go through the counter
- Badger GetFilesystemStatistics still scans for file count (cached) but reads bytes from atomic counter for freshness
- Postgres uses SQL SUM for initialization, then atomic deltas for O(1) runtime reads

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- QuotaBytes field ready for quota enforcement logic (70-02)
- GetUsedBytes() and atomic counters ready for protocol-level usage reporting (70-03)
- All downstream plans can depend on O(1) usage reads

## Self-Check: PASSED

All 13 files verified present. Both task commits (25e7a19d, 79b581c8) found. Key content markers (QuotaBytes, localStats.DiskUsed, usedBytes) confirmed in all target files.

---
*Phase: 70-storage-observability-quotas*
*Completed: 2026-03-21*
