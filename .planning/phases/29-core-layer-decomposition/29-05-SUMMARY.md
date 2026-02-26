---
phase: 29-core-layer-decomposition
plan: 05
subsystem: payload, metadata
tags: [refactoring, sub-package, conformance-tests, interface-composition, go-packages]

# Dependency graph
requires:
  - phase: 29-02
    provides: "offloader package with split files"
  - phase: 29-03
    provides: "metadata file splits establishing flat-split pattern"
provides:
  - "pkg/payload/io/ sub-package with read/write I/O operations"
  - "PayloadService composite embedding io.ServiceImpl"
  - "pkg/metadata/storetest/ conformance test suite (16 tests across 3 categories)"
  - "Conformance test wiring for memory, badger, and postgres stores"
affects: [29-06, 29-07]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "io sub-package with local interfaces to avoid circular imports"
    - "sentinel error wiring via package-level variables and init()"
    - "conformance test suite with StoreFactory pattern"

key-files:
  created:
    - pkg/payload/io/doc.go
    - pkg/payload/io/read.go
    - pkg/payload/io/write.go
    - pkg/metadata/storetest/doc.go
    - pkg/metadata/storetest/suite.go
    - pkg/metadata/storetest/file_ops.go
    - pkg/metadata/storetest/dir_ops.go
    - pkg/metadata/storetest/permissions.go
    - pkg/metadata/store/memory/memory_conformance_test.go
    - pkg/metadata/store/badger/badger_conformance_test.go
    - pkg/metadata/store/postgres/postgres_conformance_test.go
  modified:
    - pkg/payload/service.go

key-decisions:
  - "Local interfaces (CacheReader, CacheWriter, CacheStateManager, BlockDownloader, BlockUploader) in io sub-package to avoid circular imports with cache and offloader packages"
  - "Sentinel error wiring via package-level variables (CacheFileNotFoundError, CacheFullError) set in parent package init() function"
  - "PayloadService embeds *payloadio.ServiceImpl (pointer embedding) for transparent delegation of read/write methods"
  - "StoreFactory pattern: func(t *testing.T) metadata.MetadataStore allows stores to use t.TempDir() and t.Cleanup()"
  - "Badger and postgres conformance tests use integration build tag to match existing test convention"

patterns-established:
  - "io sub-package with local interfaces: define narrow interfaces in sub-package, wire concrete types from parent to avoid circular imports"
  - "Sentinel error bridging: use package-level error variables set via init() to check errors from packages that cannot be imported directly"
  - "Conformance test suite: shared test functions accepting StoreFactory, each store wires via a single test file"

requirements-completed: [REF-06.6, REF-06.7]

# Metrics
duration: 9min
completed: 2026-02-26
---

# Phase 29 Plan 05: PayloadService I/O Extraction + Metadata Store Conformance Suite Summary

**PayloadService read/write ops extracted to pkg/payload/io/ with local interfaces, plus 16-test conformance suite for metadata stores in pkg/metadata/storetest/**

## Performance

- **Duration:** 9 min
- **Started:** 2026-02-26T11:31:33Z
- **Completed:** 2026-02-26T11:41:12Z
- **Tasks:** 2
- **Files modified:** 12

## Accomplishments
- PayloadService I/O operations (ReadAt, WriteAt, Truncate, Delete, etc.) extracted to pkg/payload/io/ sub-package
- PayloadService is now a composite embedding *payloadio.ServiceImpl for transparent I/O delegation
- Local interfaces in io sub-package avoid circular imports with cache and offloader packages
- Metadata store conformance test suite with 16 tests across 3 categories (FileOps, DirOps, Permissions)
- Memory store passes all conformance tests; badger and postgres wired with integration build tag

## Task Commits

Each task was committed atomically:

1. **Task 1: Extract PayloadService I/O to pkg/payload/io/ sub-package** - `a88650b5` (refactor)
2. **Task 2: Create metadata store conformance test suite** - `feec587c` (feat)

## Files Created/Modified
- `pkg/payload/io/doc.go` - Package documentation for io sub-package
- `pkg/payload/io/read.go` - ReadAt, ReadAtWithCOWSource, GetSize, Exists + local interfaces (CacheReader, CacheWriter, CacheStateManager, BlockDownloader, BlockUploader)
- `pkg/payload/io/write.go` - WriteAt, writeBlockWithRetry, Truncate, Delete with backpressure retry
- `pkg/payload/service.go` - Updated to embed *payloadio.ServiceImpl, wire interfaces via init()
- `pkg/metadata/storetest/doc.go` - Package documentation for conformance suite
- `pkg/metadata/storetest/suite.go` - RunConformanceSuite runner + helper functions
- `pkg/metadata/storetest/file_ops.go` - 8 file operation tests
- `pkg/metadata/storetest/dir_ops.go` - 5 directory operation tests
- `pkg/metadata/storetest/permissions.go` - 3 permission attribute tests
- `pkg/metadata/store/memory/memory_conformance_test.go` - Memory store wiring
- `pkg/metadata/store/badger/badger_conformance_test.go` - BadgerDB store wiring (integration tag)
- `pkg/metadata/store/postgres/postgres_conformance_test.go` - PostgreSQL store wiring (integration tag)

## Decisions Made
- Used local interfaces in io sub-package rather than importing cache/offloader packages, following the same pattern as Plan 03's flat file split but at the sub-package level
- Sentinel error variables (CacheFileNotFoundError, CacheFullError) are wired via init() in the parent payload package, bridging error detection without direct imports
- The io.ServiceImpl accepts 5 interface parameters (CacheReader, CacheWriter, CacheStateManager, BlockDownloader, BlockUploader) which the parent PayloadService's constructor satisfies from *cache.Cache and *offloader.Offloader
- Conformance tests use StoreFactory pattern (func(t *testing.T) MetadataStore) to allow store-specific setup like temp directories and cleanup

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- PayloadService I/O extraction complete, ready for offloader/gc extraction in Plan 06
- Conformance test suite ready to be expanded with additional tests as needed
- All existing tests pass, full build succeeds, go vet clean

## Self-Check: PASSED

All 12 created files verified present. Both task commits (a88650b5, feec587c) verified in git log.

---
*Phase: 29-core-layer-decomposition*
*Completed: 2026-02-26*
