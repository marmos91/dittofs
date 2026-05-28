---
phase: 41-block-state-enum-and-listfileblocks
plan: 01
subsystem: metadata
tags: [block-state, refactor, naming, metadata-store, cache, offloader]

# Dependency graph
requires: []
provides:
  - "BlockState enum with Local/Syncing/Remote constants (values 1/2/3)"
  - "ListLocalBlocks and ListRemoteBlocks interface methods on FileBlockStore"
  - "IsRemote() and IsLocal() helpers on FileBlock"
  - "Cache methods: MarkBlockRemote, MarkBlockSyncing, MarkBlockLocal, WriteFromRemote"
  - "BadgerDB fb-local: secondary index prefix"
  - "Postgres migration 000006 renaming partial indexes"
affects: [42-blockstore-interface, 43-local-blockstore, 44-remote-blockstore, 45-per-share-wiring]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Two-tier block state naming: Dirty -> Local -> Syncing -> Remote"

key-files:
  created:
    - "pkg/metadata/store/postgres/migrations/000006_rename_block_states.up.sql"
    - "pkg/metadata/store/postgres/migrations/000006_rename_block_states.down.sql"
  modified:
    - "pkg/metadata/object.go"
    - "pkg/metadata/store.go"
    - "pkg/metadata/store/memory/objects.go"
    - "pkg/metadata/store/badger/objects.go"
    - "pkg/metadata/store/postgres/objects.go"
    - "pkg/cache/cache.go"
    - "pkg/cache/flush.go"
    - "pkg/cache/write.go"
    - "pkg/cache/recovery.go"
    - "pkg/cache/eviction.go"
    - "pkg/payload/offloader/upload.go"
    - "pkg/payload/offloader/download.go"
    - "pkg/payload/offloader/dedup.go"
    - "pkg/payload/offloader/offloader.go"
    - "pkg/payload/offloader/queue.go"
    - "pkg/metadata/store/memory/objects_test.go"

key-decisions:
  - "Kept numeric values unchanged (0-3) to avoid data migration"
  - "Log messages updated to sync terminology now, method/file renames deferred to Phase 45"

patterns-established:
  - "Block state lifecycle: Dirty -> Local -> Syncing -> Remote"
  - "Local = complete block on disk, eligible for sync to remote"
  - "Remote = confirmed in remote store, eligible for eviction"

requirements-completed: [STATE-01, STATE-02, STATE-03, STATE-04, STATE-06]

# Metrics
duration: 11min
completed: 2026-03-09
---

# Phase 41 Plan 01: Block State Enum and ListFileBlocks Summary

**Renamed block state enum from Sealed/Uploading/Uploaded to Local/Syncing/Remote across all store implementations, cache, and offloader packages**

## Performance

- **Duration:** 11 min
- **Started:** 2026-03-09T12:07:26Z
- **Completed:** 2026-03-09T12:18:56Z
- **Tasks:** 2
- **Files modified:** 18

## Accomplishments
- BlockState constants renamed: Local(1), Syncing(2), Remote(3) with updated String() method
- FileBlockStore interface: ListPendingUpload -> ListLocalBlocks, ListEvictable -> ListRemoteBlocks
- All 3 store implementations updated (memory, badger, postgres) including transaction wrappers
- Cache methods renamed: MarkBlockRemote/Syncing/Local, WriteFromRemote
- BadgerDB secondary index prefix changed from fb-sealed: to fb-local:
- Postgres migration 000006 renames partial indexes
- All log messages and comments updated to new terminology
- Zero remaining references to old names verified by grep

## Task Commits

Each task was committed atomically:

1. **Task 1: Rename types, interfaces, and store implementations** - `95bc028a` (refactor)
2. **Task 2: Update all consumers in cache and offloader packages** - `a198d8f9` (refactor)

## Files Created/Modified
- `pkg/metadata/object.go` - BlockState enum, IsRemote(), IsLocal() helpers
- `pkg/metadata/store.go` - FileBlockStore interface with ListLocalBlocks, ListRemoteBlocks
- `pkg/metadata/store/memory/objects.go` - Memory store implementation
- `pkg/metadata/store/badger/objects.go` - BadgerDB implementation with fb-local: index
- `pkg/metadata/store/postgres/objects.go` - Postgres implementation with SQL comments
- `pkg/metadata/store/postgres/migrations/000006_rename_block_states.up.sql` - Index rename migration
- `pkg/metadata/store/postgres/migrations/000006_rename_block_states.down.sql` - Rollback migration
- `pkg/cache/cache.go` - MarkBlockRemote/Syncing/Local, WriteFromRemote
- `pkg/cache/flush.go` - Dirty->Local transition, comments
- `pkg/cache/write.go` - Remote state for direct write, Local for new blocks
- `pkg/cache/recovery.go` - Syncing->Local revert, syncsReverted log key
- `pkg/cache/eviction.go` - ListRemoteBlocks call
- `pkg/payload/offloader/upload.go` - ListLocalBlocks, sync terminology in logs
- `pkg/payload/offloader/download.go` - WriteFromRemote calls
- `pkg/payload/offloader/dedup.go` - BlockStateRemote, MarkBlockRemote
- `pkg/payload/offloader/offloader.go` - Periodic syncer log messages
- `pkg/payload/offloader/queue.go` - Fetch/Sync worker log messages
- `pkg/metadata/store/memory/objects_test.go` - IsRemote() assertions

## Decisions Made
- Kept numeric values unchanged (0=Dirty, 1=Local, 2=Syncing, 3=Remote) to avoid data migration -- existing persisted FileBlock data remains valid
- Updated log messages to sync terminology now, but deferred method/file renames (e.g., uploadFileBlock -> syncFileBlock) to Phase 45 per plan

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- All block state constants use new two-tier terminology
- Ready for Phase 41 Plan 02 (ListFileBlocks query and additional store tests)
- Foundation laid for Phase 42+ where BlockStore interface and per-share wiring are built

## Self-Check: PASSED

All 18 files verified on disk. Both commits (95bc028a, a198d8f9) verified in git log.

---
*Phase: 41-block-state-enum-and-listfileblocks*
*Completed: 2026-03-09*
