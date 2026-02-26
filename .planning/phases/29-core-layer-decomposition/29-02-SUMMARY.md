---
phase: 29-core-layer-decomposition
plan: 02
subsystem: payload
tags: [offloader, gc, refactor, transfer-manager, block-store]

# Dependency graph
requires:
  - phase: 29-01
    provides: foundational error types and generic helpers
provides:
  - Offloader package (pkg/payload/offloader/) with split single-responsibility files
  - Standalone GC package (pkg/payload/gc/) with zero coupling to Offloader
  - Clean break from old pkg/payload/transfer/ package
affects: [29-03, 29-04, 29-05, 29-06, 29-07]

# Tech tracking
tech-stack:
  added: []
  patterns: [single-responsibility file split, standalone GC package, package-level type naming]

key-files:
  created:
    - pkg/payload/offloader/offloader.go
    - pkg/payload/offloader/upload.go
    - pkg/payload/offloader/download.go
    - pkg/payload/offloader/dedup.go
    - pkg/payload/offloader/queue.go
    - pkg/payload/offloader/entry.go
    - pkg/payload/offloader/types.go
    - pkg/payload/offloader/wal_replay.go
    - pkg/payload/offloader/doc.go
    - pkg/payload/offloader/offloader_test.go
    - pkg/payload/offloader/queue_test.go
    - pkg/payload/offloader/entry_test.go
    - pkg/payload/offloader/wal_replay_test.go
    - pkg/payload/gc/gc.go
    - pkg/payload/gc/gc_test.go
    - pkg/payload/gc/gc_integration_test.go
    - pkg/payload/gc/doc.go
  modified:
    - pkg/payload/service.go
    - pkg/payload/types.go
    - pkg/payload/service_test.go
    - pkg/controlplane/runtime/init.go
    - internal/adapter/nfs/v3/handlers/testing/fixtures.go
    - internal/adapter/nfs/v4/handlers/io_test.go
    - pkg/payload/store/blockstore_integration_test.go

key-decisions:
  - "Split 1361-line manager.go into 8 focused files by responsibility"
  - "Renamed TransferManager to Offloader (better describes cache-to-store offloading)"
  - "Extracted GC to standalone pkg/payload/gc/ with zero coupling to Offloader"
  - "GC types renamed: GCStats->Stats, GCOptions->Options (package context eliminates stuttering)"
  - "parseShareName duplicated in both gc and offloader packages (each needs it independently)"
  - "MetadataReconciler interface duplicated in gc package for zero-coupling design"

patterns-established:
  - "Single-responsibility file naming: offloader.go (orchestration), upload.go (uploads), download.go (downloads), dedup.go (deduplication)"
  - "Package-level type naming: gc.Stats not gc.GCStats"

requirements-completed: [REF-05.5, REF-05.6, REF-05.7, REF-05.8]

# Metrics
duration: 21min
completed: 2026-02-26
---

# Phase 29 Plan 02: Offloader Rename/Split Summary

**Renamed TransferManager to Offloader, split 1361-line god object into 8 focused files, extracted GC to standalone package, updated all 7 importers**

## Performance

- **Duration:** ~21 min
- **Started:** 2026-02-26T10:00:00Z
- **Completed:** 2026-02-26T10:21:00Z
- **Tasks:** 2
- **Files modified:** 28 (17 created, 7 modified, 12 deleted)

## Accomplishments
- Split pkg/payload/transfer/manager.go (1361 lines) into 8 focused files in pkg/payload/offloader/
- Extracted GC to standalone pkg/payload/gc/ package with zero coupling to Offloader
- Updated all 7 importer files across the codebase (service, runtime, handlers, tests)
- Deleted pkg/payload/transfer/ directory entirely - clean break, no aliases
- All unit tests pass: offloader (1.6s), gc (1.3s), payload (1.1s), plus all adapter tests

## Task Commits

Each task was committed atomically:

1. **Task 1: Move transfer/ to offloader/, rename TransferManager, split manager.go** - `a5099bbd` (refactor)
2. **Task 2: Extract GC, update importers, remove transfer/** - `f4fe8798` (refactor)

## Files Created/Modified

### Created (pkg/payload/offloader/)
- `doc.go` - Package documentation
- `offloader.go` - Offloader struct, constructor, Flush, Close, Start, orchestration
- `upload.go` - OnWriteComplete, tryEagerUpload, uploadBlock, uploadRemainingBlocks
- `download.go` - downloadBlock, EnsureAvailable, enqueueDownload, enqueuePrefetch
- `dedup.go` - Upload state tracking, finalization callbacks, DeleteWithRefCount
- `queue.go` - TransferQueue with priority scheduling (downloads > uploads > prefetch)
- `entry.go` - TransferRequest, FormatBlockKey constructors
- `types.go` - Config, FlushResult, TransferType, TransferQueueConfig
- `wal_replay.go` - RecoverUnflushedBlocks, ReconcileMetadata, parseShareName
- `offloader_test.go` - Integration tests (renamed from manager_test.go)
- `queue_test.go` - Queue unit tests
- `entry_test.go` - Entry unit tests
- `wal_replay_test.go` - WAL replay and parseShareName tests

### Created (pkg/payload/gc/)
- `doc.go` - Package documentation
- `gc.go` - CollectGarbage, Stats, Options, MetadataReconciler, parsePayloadIDFromBlockKey
- `gc_test.go` - Unit tests with memory stores
- `gc_integration_test.go` - Integration tests with filesystem and S3 (Localstack)

### Modified (importers)
- `pkg/payload/service.go` - transfer.TransferManager -> offloader.Offloader
- `pkg/payload/types.go` - transfer.FlushResult -> offloader.FlushResult
- `pkg/payload/service_test.go` - transfer.New/DefaultConfig -> offloader.New/DefaultConfig
- `pkg/controlplane/runtime/init.go` - transfer.Config/New -> offloader.Config/New
- `internal/adapter/nfs/v3/handlers/testing/fixtures.go` - transfer -> offloader imports
- `internal/adapter/nfs/v4/handlers/io_test.go` - transfer -> offloader imports
- `pkg/payload/store/blockstore_integration_test.go` - transfer -> offloader imports

### Deleted
- Entire `pkg/payload/transfer/` directory (12 files)

## Decisions Made
- Split manager.go by responsibility: offloader.go (orchestration/lifecycle), upload.go (upload methods), download.go (download methods), dedup.go (dedup/finalization)
- GC types renamed to avoid stuttering: gc.Stats instead of gc.GCStats
- parseShareName duplicated in both gc and offloader (each needs it independently, keeps zero coupling)
- MetadataReconciler interface duplicated in gc package for isolation

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed logger.Errorf used as error return in dedup.go**
- **Found during:** Task 1 (file split)
- **Issue:** `return logger.Errorf("offloader is closed")` - logger.Errorf returns void, not error
- **Fix:** Changed to `return fmt.Errorf("offloader is closed")` and added `"fmt"` import
- **Files modified:** pkg/payload/offloader/dedup.go
- **Verification:** go build succeeds
- **Committed in:** a5099bbd (Task 1 commit)

**2. [Rule 3 - Blocking] Removed unnecessary crypto/sha256 import from offloader.go**
- **Found during:** Task 1 (file split)
- **Issue:** offloader.go had `import "crypto/sha256"` and `var _ = sha256.Sum256` but sha256 is only used in upload.go
- **Fix:** Removed unused import and dummy usage
- **Files modified:** pkg/payload/offloader/offloader.go
- **Verification:** go build succeeds
- **Committed in:** a5099bbd (Task 1 commit)

---

**Total deviations:** 2 auto-fixed (1 bug, 1 blocking)
**Impact on plan:** Both fixes necessary for compilation. No scope creep.

## Issues Encountered
None - plan executed smoothly.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Offloader package ready for further decomposition in subsequent plans
- GC package fully standalone, can be enhanced independently
- All import paths updated, no legacy references remain in Go source files

## Self-Check: PASSED

- All 17 created files verified present
- pkg/payload/transfer/ confirmed deleted
- Both commits (a5099bbd, f4fe8798) verified in git log
- go build ./... succeeds
- go test ./pkg/payload/... passes all packages

---
*Phase: 29-core-layer-decomposition*
*Completed: 2026-02-26*
