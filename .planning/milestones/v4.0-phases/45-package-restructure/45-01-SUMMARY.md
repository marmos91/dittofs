---
phase: 45-package-restructure
plan: 01
subsystem: storage
tags: [blockstore, interfaces, types, refactoring, go]

# Dependency graph
requires: []
provides:
  - "pkg/blockstore/ type hierarchy (FileBlock, BlockState, ContentHash, BlockSize)"
  - "pkg/blockstore/ interface definitions (FileBlockStore, Store, Reader, Writer, Flusher)"
  - "pkg/blockstore/local/LocalStore with 4 sub-interfaces"
  - "pkg/blockstore/remote/RemoteStore interface"
  - "pkg/blockstore/ error types (BlockStoreError, sentinel errors)"
  - "pkg/blockstore/storetest/ FileBlockStore conformance suite"
  - "metadata type aliases pointing to blockstore (backward compatible)"
affects: [45-02, 45-03, 45-04, cache-refactor, payload-refactor, offloader-refactor]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Type alias pattern for backward-compatible package extraction"
    - "Conformance suite delegation across packages"

key-files:
  created:
    - pkg/blockstore/doc.go
    - pkg/blockstore/types.go
    - pkg/blockstore/errors.go
    - pkg/blockstore/store.go
    - pkg/blockstore/local/local.go
    - pkg/blockstore/local/doc.go
    - pkg/blockstore/remote/remote.go
    - pkg/blockstore/remote/doc.go
    - pkg/blockstore/storetest/file_block_ops.go
    - pkg/blockstore/storetest/doc.go
  modified:
    - pkg/metadata/object.go
    - pkg/metadata/store.go
    - pkg/metadata/storetest/file_block_ops.go
    - pkg/metadata/store/memory/objects.go
    - pkg/metadata/store/badger/objects.go
    - pkg/metadata/store/postgres/objects.go

key-decisions:
  - "Type aliases in metadata/object.go for backward compatibility -- avoids touching all consumers"
  - "FileBlockStore type alias in metadata/store.go -- Transaction/MetadataStore interfaces embed via alias"
  - "blockstore is a leaf dependency -- no imports from metadata, preventing circular deps"
  - "ErrInvalidHash and ErrFileBlockNotFound moved to blockstore as simple errors.New sentinel errors"

patterns-established:
  - "Bottom-up extraction: types/interfaces first, then update consumers via aliases"
  - "Conformance suite delegation: metadata/storetest delegates FileBlockOps to blockstore/storetest"

requirements-completed: [PKG-01, PKG-02]

# Metrics
duration: 6min
completed: 2026-03-09
---

# Phase 45 Plan 01: Package Restructure - Type Hierarchy Summary

**Created pkg/blockstore/ with FileBlock/BlockState/ContentHash types, FileBlockStore/Store/LocalStore/RemoteStore interfaces, BlockStoreError, and conformance suite; metadata package uses type aliases for backward compatibility**

## Performance

- **Duration:** 6 min
- **Started:** 2026-03-09T18:49:00Z
- **Completed:** 2026-03-09T18:55:42Z
- **Tasks:** 2
- **Files modified:** 16

## Accomplishments
- Created pkg/blockstore/ with single-source-of-truth types: FileBlock, BlockState, ContentHash, BlockSize, FormatStoreKey, BlockRef
- Defined FileBlockStore interface (10 methods) and composed Store interface with Reader/Writer/Flusher sub-interfaces
- Defined LocalStore with 4 sub-interfaces (LocalReader, LocalWriter, LocalFlusher, LocalManager) covering all BlockCache public methods
- Defined RemoteStore interface matching current BlockStore's 8 methods
- Created BlockStoreError (renamed from PayloadError) with all 14 sentinel errors
- Updated metadata package to use type aliases pointing to blockstore -- zero consumer breakage
- Moved FileBlockStore conformance tests to blockstore/storetest, metadata delegates to it

## Task Commits

Each task was committed atomically:

1. **Task 1: Create pkg/blockstore/ root types, interfaces, and errors** - `b4e3e83c` (feat)
2. **Task 2: Update metadata package to import from blockstore and move conformance tests** - `a7b8f9df` (feat)

## Files Created/Modified
- `pkg/blockstore/doc.go` - Package documentation
- `pkg/blockstore/types.go` - FileBlock, BlockState, ContentHash, BlockSize, FormatStoreKey, BlockRef
- `pkg/blockstore/errors.go` - BlockStoreError, 14 sentinel errors, ErrFileBlockNotFound, ErrInvalidHash
- `pkg/blockstore/store.go` - FileBlockStore, Store, Reader, Writer, Flusher, FlushResult, Stats
- `pkg/blockstore/local/local.go` - LocalStore, LocalReader, LocalWriter, LocalFlusher, LocalManager, PendingBlock, FlushedBlock, Stats
- `pkg/blockstore/local/doc.go` - Package documentation
- `pkg/blockstore/remote/remote.go` - RemoteStore interface (8 methods)
- `pkg/blockstore/remote/doc.go` - Package documentation
- `pkg/blockstore/storetest/file_block_ops.go` - FileBlockStore conformance suite
- `pkg/blockstore/storetest/doc.go` - Package documentation
- `pkg/metadata/object.go` - Replaced local types with aliases to blockstore
- `pkg/metadata/store.go` - Replaced FileBlockStore interface with alias to blockstore.FileBlockStore
- `pkg/metadata/storetest/file_block_ops.go` - Delegates to blockstore/storetest
- `pkg/metadata/store/memory/objects.go` - Updated interface assertion to blockstore.FileBlockStore
- `pkg/metadata/store/badger/objects.go` - Updated interface assertion to blockstore.FileBlockStore
- `pkg/metadata/store/postgres/objects.go` - Updated interface assertion to blockstore.FileBlockStore

## Decisions Made
- Used Go type aliases (`type X = Y`) instead of named types to avoid breaking downstream consumers
- Kept ErrInvalidHash and ErrFileBlockNotFound as simple `errors.New` sentinels in blockstore (not StoreError wrappers) since they're used for control flow checks with `errors.Is()`
- LocalStore sub-interface method signatures match BlockCache public methods exactly, including `ExistsOnDisk(ctx, payloadID, blockIdx) (bool, error)` return signature
- blockstore package has zero imports from metadata -- verified with grep, ensuring no circular dependencies

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- pkg/blockstore/ type hierarchy is complete and ready for Plans 02-04 to build on
- Plan 02 can implement LocalStore (cache refactor) using the local.LocalStore interface
- Plan 03 can implement RemoteStore (payload store refactor) using the remote.RemoteStore interface
- Plan 04 can wire up the composed Store interface

## Self-Check: PASSED

- All 16 files verified present on disk
- Both task commits (b4e3e83c, a7b8f9df) verified in git log
- `go build ./...` succeeds (entire project compiles)
- `go test ./pkg/metadata/...` passes (all metadata tests)
- `go vet ./pkg/blockstore/...` passes (no vet issues)
- No circular imports (blockstore has zero imports from metadata)

---
*Phase: 45-package-restructure*
*Completed: 2026-03-09*
