---
phase: 45-package-restructure
plan: 02
subsystem: blockstore
tags: [go, blockstore, local-store, remote-store, sync, gc, s3, memory, conformance-tests]

# Dependency graph
requires:
  - phase: 45-package-restructure-01
    provides: blockstore type hierarchy (interfaces, types, FileBlockStore)
provides:
  - FSStore at pkg/blockstore/local/fs/ implementing local.LocalStore
  - MemoryLocalStore at pkg/blockstore/local/memory/ implementing local.LocalStore
  - S3Store at pkg/blockstore/remote/s3/ implementing remote.RemoteStore
  - MemoryRemoteStore at pkg/blockstore/remote/memory/ implementing remote.RemoteStore
  - Syncer at pkg/blockstore/sync/ (renamed from Offloader)
  - GC at pkg/blockstore/gc/ with CollectGarbage and CollectUnreferenced
  - LocalStore conformance suite at pkg/blockstore/local/localtest/
  - RemoteStore conformance suite at pkg/blockstore/remote/remotetest/
affects: [45-package-restructure-03, 45-package-restructure-04]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "gosync alias for Go sync in sync package"
    - "Conformance test suites with factory pattern"
    - "Interface satisfaction compile-time checks (var _ Interface = (*Impl)(nil))"

key-files:
  created:
    - pkg/blockstore/local/fs/fs.go
    - pkg/blockstore/local/fs/read.go
    - pkg/blockstore/local/fs/write.go
    - pkg/blockstore/local/fs/flush.go
    - pkg/blockstore/local/fs/eviction.go
    - pkg/blockstore/local/fs/manage.go
    - pkg/blockstore/local/memory/memory.go
    - pkg/blockstore/local/localtest/suite.go
    - pkg/blockstore/remote/memory/store.go
    - pkg/blockstore/remote/s3/store.go
    - pkg/blockstore/remote/remotetest/suite.go
    - pkg/blockstore/sync/syncer.go
    - pkg/blockstore/sync/sync.go
    - pkg/blockstore/sync/fetch.go
    - pkg/blockstore/sync/dedup.go
    - pkg/blockstore/sync/queue.go
    - pkg/blockstore/sync/entry.go
    - pkg/blockstore/sync/types.go
    - pkg/blockstore/gc/gc.go
  modified: []

key-decisions:
  - "Used gosync alias for Go standard sync in pkg/blockstore/sync/ to avoid package name collision"
  - "Tests use fs.FSStore instead of cache.BlockCache since old cache not yet updated to new interfaces"
  - "testEnv struct uses local.LocalStore interface type for cache field enabling test portability"

patterns-established:
  - "gosync alias: import gosync \"sync\" in any package named sync"
  - "Factory-based conformance test suites: RunConformanceSuite(t, factory) for interface testing"

requirements-completed: [PKG-03, PKG-04, PKG-05, PKG-06, PKG-07, PKG-08]

# Metrics
duration: 45min
completed: 2026-03-09
---

# Phase 45 Plan 02: Implementation Move Summary

**Moved cache to local/fs, payload stores to remote/, offloader to sync/ (Syncer rename), gc to gc/ with conformance test suites for both local and remote interfaces**

## Performance

- **Duration:** ~45 min (across 2 sessions with context continuation)
- **Started:** 2026-03-09T18:55:42Z
- **Completed:** 2026-03-09T20:33:00Z
- **Tasks:** 2
- **Files created:** 47 (21 in Task 1, 21 in Task 2, plus 5 doc files)

## Accomplishments
- Complete implementation hierarchy under pkg/blockstore/ with 6 sub-packages (local/fs, local/memory, remote/memory, remote/s3, sync, gc)
- FSStore (renamed from BlockCache) and MemoryLocalStore both satisfy local.LocalStore with compile-time checks
- S3Store and MemoryRemoteStore both satisfy remote.RemoteStore with compile-time checks
- Syncer (renamed from Offloader) compiles with all method/type renames applied using gosync alias
- Conformance suites for both LocalStore (localtest) and RemoteStore (remotetest)
- All 75+ unit tests pass across the new hierarchy

## Task Commits

Each task was committed atomically:

1. **Task 1: Move cache to local/fs, create memory local store, and local conformance suite** - `e48bb670` (feat)
2. **Task 2: Move remote stores, offloader to sync, GC, and create remote conformance suite** - `49aad56f` (feat)

## Files Created/Modified

### pkg/blockstore/local/fs/ (moved from pkg/cache/)
- `fs.go` - FSStore struct (renamed from BlockCache) implementing local.LocalStore
- `read.go` - ReadAt implementation with memory-then-disk fallback
- `write.go` - WriteAt with 8MB block buffering and direct-disk optimization
- `flush.go` - Flush and GetDirtyBlocks for syncer integration
- `eviction.go` - LRU disk eviction for remote-backed blocks
- `manage.go` - Lifecycle, deletion, truncation, state transitions
- `recovery.go` - WAL replay for crash recovery
- `block.go` - memBlock type and sync.Pool buffer management
- `types.go` - Internal types (blockKey, fileInfo, fdCache params)
- `fdcache.go` - File descriptor cache for read/write hot paths
- `fadvise_linux.go` / `fadvise_other.go` - Platform-specific page cache advice
- `fs_test.go` - 30+ unit tests including conformance suite
- `manage_test.go` - Manager interface tests
- `doc.go` - Package documentation

### pkg/blockstore/local/memory/
- `memory.go` - Full MemoryStore implementing local.LocalStore (in-memory maps)
- `memory_test.go` - Unit tests + conformance suite
- `doc.go` - Package documentation

### pkg/blockstore/local/localtest/
- `suite.go` - 9-test conformance suite (write/read round-trip, flush, truncate, etc.)
- `doc.go` - Package documentation

### pkg/blockstore/remote/memory/
- `store.go` - In-memory RemoteStore for testing
- `store_test.go` - 10 unit tests

### pkg/blockstore/remote/s3/
- `store.go` - S3-backed RemoteStore with multipart, range reads, retry

### pkg/blockstore/remote/remotetest/
- `suite.go` - 9-test RemoteStore conformance suite
- `doc.go` - Package documentation

### pkg/blockstore/sync/ (moved from pkg/payload/offloader/)
- `syncer.go` - Main Syncer struct (renamed from Offloader)
- `sync.go` - syncLocalBlocks, syncFileBlock, uploadBlock (renamed from upload.go)
- `fetch.go` - fetchBlock, EnsureAvailable, EnsureAvailableAndRead (renamed from download.go)
- `dedup.go` - getSyncState, DeleteWithRefCount
- `queue.go` - SyncQueue (renamed from TransferQueue)
- `entry.go` - TransferRequest types
- `types.go` - Config, FlushResult, ErrClosed, BlockSize
- `doc.go` - Package documentation with gosync alias note
- `syncer_test.go` - Integration tests (build-tagged)
- `nil_remotestore_test.go` - 15 nil-remote-store tests
- `queue_test.go` - Queue unit tests
- `entry_test.go` - Entry unit tests

### pkg/blockstore/gc/ (moved from pkg/payload/gc/)
- `gc.go` - CollectGarbage and CollectUnreferenced
- `gc_test.go` - 7 unit tests
- `gc_integration_test.go` - S3 integration tests (build-tagged)
- `doc.go` - Package documentation

## Decisions Made

1. **gosync alias for sync package**: Since the package is named `sync`, Go's standard `sync` must be aliased as `gosync` throughout all files in that package. This is documented in doc.go.
2. **Tests use fs.FSStore not cache.BlockCache**: The old `cache.BlockCache` doesn't implement the new `local.LocalStore` interface (different return types on Flush). Test helpers were updated to use `fs.New()` from the new hierarchy.
3. **testEnv.cache field uses local.LocalStore interface type**: Enables test portability between fs and memory implementations.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Fixed nil_remotestore_test.go and syncer_test.go type mismatch**
- **Found during:** Task 2 (sync package tests)
- **Issue:** Tests used `*cache.BlockCache` which doesn't satisfy `local.LocalStore` (Flush returns `[]cache.FlushedBlock` not `[]local.FlushedBlock`)
- **Fix:** Replaced `cache.New()` with `fs.New()` from `pkg/blockstore/local/fs/` in all test helpers. Changed `testEnv.cache` field type from `*cache.BlockCache` to `local.LocalStore`.
- **Files modified:** `pkg/blockstore/sync/nil_remotestore_test.go`, `pkg/blockstore/sync/syncer_test.go`
- **Verification:** `go test ./pkg/blockstore/sync/` -- 29/29 PASS
- **Committed in:** `49aad56f` (part of Task 2 commit)

---

**Total deviations:** 1 auto-fixed (1 blocking)
**Impact on plan:** Necessary fix for interface type mismatch between old and new packages. No scope creep.

## Issues Encountered
None beyond the auto-fixed deviation above.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Complete blockstore implementation hierarchy exists under pkg/blockstore/
- All implementations satisfy their interfaces with compile-time checks
- Conformance suites verify interface contracts
- Ready for Plan 03 (runtime wiring) and Plan 04 (consumer import updates and old package deletion)

## Self-Check: PASSED

- All 10 key files verified present on disk
- Both task commits (e48bb670, 49aad56f) verified in git history
- All test suites pass: local/fs, local/memory, remote/memory, sync, gc

---
*Phase: 45-package-restructure*
*Completed: 2026-03-09*
