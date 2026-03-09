---
phase: 41-block-state-enum-and-listfileblocks
plan: 02
subsystem: metadata
tags: [file-block, query, conformance-tests, metadata-store, dedup]

# Dependency graph
requires:
  - phase: 41-01
    provides: "BlockState enum with Local/Syncing/Remote, ListLocalBlocks and ListRemoteBlocks interface methods"
provides:
  - "ListFileBlocks(ctx, payloadID) method on FileBlockStore interface"
  - "Memory, BadgerDB, PostgreSQL implementations of ListFileBlocks"
  - "BadgerDB fb-file: secondary index for per-file block queries"
  - "Comprehensive FileBlockStore conformance tests (11 tests)"
affects: [42-blockstore-interface, 43-local-blockstore, 44-remote-blockstore, 45-per-share-wiring]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Per-file block queries via payloadID prefix scan"
    - "BadgerDB secondary index pattern: fb-file:{payloadID}:{blockIdx}"
    - "Numeric block index sorting in Go after lexicographic DB fetch"

key-files:
  created:
    - "pkg/metadata/storetest/file_block_ops.go"
  modified:
    - "pkg/metadata/store.go"
    - "pkg/metadata/store/memory/objects.go"
    - "pkg/metadata/store/badger/objects.go"
    - "pkg/metadata/store/postgres/objects.go"
    - "pkg/metadata/storetest/suite.go"

key-decisions:
  - "Block index sorting done in Go after DB fetch for correct numeric ordering across all stores"
  - "BadgerDB fb-file: index maintained on every PutFileBlock regardless of state"

patterns-established:
  - "ListFileBlocks returns empty slice (not nil) when no blocks found"
  - "Block IDs use {payloadID}/{blockIdx} format as convention across all stores"

requirements-completed: [STATE-05]

# Metrics
duration: 4min
completed: 2026-03-09
---

# Phase 41 Plan 02: ListFileBlocks Query and Conformance Tests Summary

**ListFileBlocks per-file block query on all 3 store implementations with 11-test conformance suite covering ListLocalBlocks, ListRemoteBlocks, and ListFileBlocks**

## Performance

- **Duration:** 4 min
- **Started:** 2026-03-09T12:26:10Z
- **Completed:** 2026-03-09T12:30:23Z
- **Tasks:** 2
- **Files modified:** 6

## Accomplishments
- ListFileBlocks method added to FileBlockStore interface with implementations in memory, badger, and postgres
- BadgerDB uses fb-file: secondary index for O(file_blocks) prefix scan queries
- 11 conformance tests covering all 3 query methods (ListLocalBlocks, ListRemoteBlocks, ListFileBlocks) with filtering, ordering, limits, mixed states, and empty store edge cases
- All transaction wrappers expose ListFileBlocks

## Task Commits

Each task was committed atomically:

1. **Task 1: Add ListFileBlocks to interface and all implementations** - `920acb99` (feat)
2. **Task 2: Add FileBlockStore conformance tests** - `7c2e65d1` (test)

## Files Created/Modified
- `pkg/metadata/store.go` - Added ListFileBlocks to FileBlockStore interface
- `pkg/metadata/store/memory/objects.go` - Memory implementation with prefix filter and index sort
- `pkg/metadata/store/badger/objects.go` - BadgerDB implementation with fb-file: secondary index
- `pkg/metadata/store/postgres/objects.go` - PostgreSQL implementation with LIKE query
- `pkg/metadata/storetest/file_block_ops.go` - 11 conformance tests for all query methods
- `pkg/metadata/storetest/suite.go` - Registered FileBlockOps in RunConformanceSuite

## Decisions Made
- Block index sorting done in Go after DB fetch to handle multi-digit indices correctly (lexicographic "10" < "2" in DB)
- BadgerDB fb-file: index maintained on every PutFileBlock call regardless of block state, ensuring ListFileBlocks always returns complete results
- ListFileBlocks returns empty slice (not nil) for consistency when no blocks found

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- All FileBlockStore query methods implemented and tested across memory and badger
- Conformance suite ready for Phase 42+ where BlockStore interface abstractions are built
- Foundation complete for per-share block management operations

## Self-Check: PASSED
