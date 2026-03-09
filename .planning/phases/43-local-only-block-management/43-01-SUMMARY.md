---
phase: 43-local-only-block-management
plan: 01
subsystem: cache
tags: [blockcache, block-management, eviction, local-only]

# Dependency graph
requires:
  - phase: 41-block-state-machine
    provides: BlockState lifecycle (Dirty/Local/Syncing/Remote) and FileBlockStore interface
provides:
  - DeleteBlockFile, DeleteAllBlockFiles, TruncateBlockFiles block lifecycle methods
  - GetStoredFileSize, ExistsOnDisk block query methods
  - SetEvictionEnabled eviction control for local-only mode
  - EvictMemory rename (was Remove) with clear semantics
affects: [43-02, 43-03, 44-blockstore-interface]

# Tech tracking
tech-stack:
  added: []
  patterns: [direct-delete-with-pendingFBs-cleanup, eviction-guard-via-atomic-bool]

key-files:
  created:
    - pkg/cache/manage.go
    - pkg/cache/manage_test.go
  modified:
    - pkg/cache/cache.go
    - pkg/cache/eviction.go
    - pkg/cache/cache_test.go
    - pkg/payload/service.go
    - pkg/payload/store/blockstore_integration_test.go

key-decisions:
  - "Direct blockStore.DeleteFileBlock call for deletes (not async pendingFBs)"
  - "pendingFBs.Delete as cleanup-only to prevent zombie re-creation"
  - "extractBlockIdx via simple numeric parsing from blockID suffix"

patterns-established:
  - "Block management methods in manage.go separate from core cache.go"
  - "evictionEnabled atomic.Bool for mode control without lock contention"

requirements-completed: [LOCAL-01, LOCAL-03]

# Metrics
duration: 5min
completed: 2026-03-09
---

# Phase 43 Plan 01: Block Management Summary

**Block lifecycle methods (delete, truncate, size, disk-check) with eviction control via atomic.Bool for local-only mode**

## Performance

- **Duration:** 5 min
- **Started:** 2026-03-09T15:21:17Z
- **Completed:** 2026-03-09T15:25:55Z
- **Tasks:** 2
- **Files modified:** 7

## Accomplishments
- Added 5 block management methods (DeleteBlockFile, DeleteAllBlockFiles, TruncateBlockFiles, GetStoredFileSize, ExistsOnDisk) and SetEvictionEnabled in new manage.go
- Added evictionEnabled atomic.Bool to BlockCache with fast path in ensureSpace for local-only mode
- Renamed Remove to EvictMemory across all callers for clearer semantics
- Verified LOCAL-03: flush still marks blocks BlockStateLocal (existing behavior preserved)
- 11 new tests covering all manage.go methods including idempotency and stale metadata

## Task Commits

Each task was committed atomically:

1. **Task 1: Add manage.go with block management methods and SetEvictionEnabled** - `db980761` (test: failing tests), `9b9ab378` (feat: implementation + green)
2. **Task 2: Rename Remove to EvictMemory across all callers** - `c8b82b9c` (refactor)

_Note: Task 1 followed TDD with RED commit then GREEN commit._

## Files Created/Modified
- `pkg/cache/manage.go` - Block lifecycle management methods (DeleteBlockFile, DeleteAllBlockFiles, TruncateBlockFiles, GetStoredFileSize, ExistsOnDisk, SetEvictionEnabled)
- `pkg/cache/manage_test.go` - Tests for all manage.go methods (11 tests)
- `pkg/cache/cache.go` - Added evictionEnabled field, initialized in New(), renamed Remove to EvictMemory
- `pkg/cache/eviction.go` - Added eviction-disabled fast path in ensureSpace
- `pkg/cache/cache_test.go` - Updated Remove call to EvictMemory
- `pkg/payload/service.go` - Updated cache.Remove to cache.EvictMemory
- `pkg/payload/store/blockstore_integration_test.go` - Updated bc.Remove to bc.EvictMemory

## Decisions Made
- Direct blockStore.DeleteFileBlock call for deletes (not async pendingFBs) -- ensures immediate consistency
- pendingFBs.Delete as cleanup-only after the direct delete -- prevents stale async update from re-creating a deleted block
- extractBlockIdx uses simple numeric parsing from blockID suffix -- avoids strconv dependency for this internal helper

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Block management methods ready for use by plans 43-02 (local block store) and 43-03 (integration)
- evictionEnabled flag ready for local-only mode configuration
- EvictMemory rename propagated to all callers

## Self-Check: PASSED

All files exist, all commits verified.

---
*Phase: 43-local-only-block-management*
*Completed: 2026-03-09*
