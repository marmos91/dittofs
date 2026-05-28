---
phase: 45-package-restructure
plan: 03
subsystem: blockstore
tags: [blockstore, orchestrator, engine, runtime, io, payloadservice-replacement]

# Dependency graph
requires:
  - phase: 45-02
    provides: "sync.Syncer, remote stores, GC collector in blockstore hierarchy"
provides:
  - "engine.BlockStore orchestrator composing local + remote + syncer"
  - "blockstore/io/ package with cache-aware read/write helpers"
  - "Runtime wired to use BlockStore instead of PayloadService"
  - "All protocol adapters (NFSv3, NFSv4, SMB) updated to use BlockStore"
affects: [45-04, runtime, adapters]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "engine sub-package pattern to avoid import cycles in Go"
    - "string() conversion at adapter boundaries for PayloadID"
    - "Deprecated method aliases for backward compatibility"

key-files:
  created:
    - pkg/blockstore/engine/engine.go
    - pkg/blockstore/io/doc.go
    - pkg/blockstore/io/read.go
    - pkg/blockstore/io/write.go
  modified:
    - pkg/controlplane/runtime/runtime.go
    - pkg/controlplane/runtime/init.go
    - pkg/controlplane/runtime/init_test.go
    - pkg/controlplane/runtime/runtime_test.go
    - internal/adapter/nfs/v3/handlers/utils.go
    - internal/adapter/nfs/v3/handlers/read.go
    - internal/adapter/nfs/v3/handlers/read_payload.go
    - internal/adapter/nfs/v3/handlers/write.go
    - internal/adapter/nfs/v3/handlers/create.go
    - internal/adapter/nfs/v3/handlers/remove.go
    - internal/adapter/nfs/v3/handlers/commit.go
    - internal/adapter/nfs/v3/handlers/testing/fixtures.go
    - internal/adapter/nfs/v4/handlers/helpers.go
    - internal/adapter/nfs/v4/handlers/read.go
    - internal/adapter/nfs/v4/handlers/write.go
    - internal/adapter/nfs/v4/handlers/commit.go
    - internal/adapter/nfs/v4/handlers/io_test.go
    - internal/adapter/smb/v2/handlers/close.go
    - internal/adapter/smb/v2/handlers/durable_scavenger.go
    - internal/adapter/smb/v2/handlers/flush.go
    - internal/adapter/smb/v2/handlers/handler.go
    - internal/adapter/smb/v2/handlers/read.go
    - internal/adapter/smb/v2/handlers/write.go
    - internal/controlplane/api/handlers/durable_handle.go

key-decisions:
  - "BlockStore orchestrator placed in pkg/blockstore/engine/ sub-package to avoid import cycle with sync/local"
  - "local/local.go uses local hashSize constant instead of importing blockstore.HashSize to break cycle"
  - "OffloaderConfig kept as type alias for SyncerConfig for backward compatibility"
  - "Deprecated method aliases retained: GetPayloadService, GetBlockService, EnsurePayloadService"
  - "string() conversion used at adapter call sites rather than changing BlockStore to accept PayloadID"

patterns-established:
  - "engine sub-package: when orchestrator can't live in root package due to import cycles, use engine/ sub-package"
  - "PayloadID to string conversion at protocol adapter boundaries"

requirements-completed: [PKG-09]

# Metrics
duration: ~35min
completed: 2025-03-09
---

# Phase 45 Plan 03: BlockStore Orchestrator Summary

**engine.BlockStore orchestrator replaces PayloadService, composing local store + remote store + syncer with full adapter wiring across NFSv3/v4/SMB**

## Performance

- **Duration:** ~35 min
- **Started:** 2025-03-09
- **Completed:** 2025-03-09
- **Tasks:** 2
- **Files modified:** 28

## Accomplishments
- Created engine.BlockStore orchestrator that satisfies blockstore.Store interface, composing local.LocalStore + remote.RemoteStore + blocksync.Syncer
- Created blockstore/io/ package with cache-aware ReadAt, ReadAtWithCOWSource, and WriteAt helpers
- Replaced PayloadService throughout runtime: renamed fields, methods, and config types
- Updated all protocol adapter code (NFSv3 handlers, NFSv4 handlers, SMB handlers, API handlers) to use engine.BlockStore
- Updated all test fixtures to construct BlockStore via engine.New instead of payload.New

## Task Commits

Each task was committed atomically:

1. **Task 1: Create BlockStore orchestrator and io/ package** - `1dee4730` (feat)
2. **Task 2: Update runtime and config to use BlockStore** - `4ee59bf3` (feat)

## Files Created/Modified

### Created
- `pkg/blockstore/engine/engine.go` - BlockStore orchestrator composing local+remote+syncer, implements blockstore.Store
- `pkg/blockstore/io/doc.go` - Package documentation for io sub-package
- `pkg/blockstore/io/read.go` - Cache-aware read operations with SyncerReader interface and COW support
- `pkg/blockstore/io/write.go` - Write delegation to local store

### Modified (Runtime)
- `pkg/controlplane/runtime/runtime.go` - Replaced payloadService field with blockStore, renamed helper types
- `pkg/controlplane/runtime/init.go` - EnsureBlockStore using fs.New + blocksync.New + engine.New
- `pkg/controlplane/runtime/init_test.go` - Updated test to TestEnsureBlockStoreLocalOnly
- `pkg/controlplane/runtime/runtime_test.go` - Updated test assertions for BlockStore

### Modified (Protocol Adapters)
- `internal/adapter/nfs/v3/handlers/utils.go` - getServices/getBlockStore return *engine.BlockStore
- `internal/adapter/nfs/v3/handlers/read.go` - Uses getBlockStore
- `internal/adapter/nfs/v3/handlers/read_payload.go` - Takes *engine.BlockStore, converts PayloadID to string
- `internal/adapter/nfs/v3/handlers/write.go` - string(writeIntent.PayloadID) conversion
- `internal/adapter/nfs/v3/handlers/create.go` - truncateExistingFile takes *engine.BlockStore
- `internal/adapter/nfs/v3/handlers/remove.go` - string(removedFileAttr.PayloadID) conversion
- `internal/adapter/nfs/v3/handlers/commit.go` - Uses getBlockStore, string conversion for Flush
- `internal/adapter/nfs/v3/handlers/testing/fixtures.go` - Constructs engine.BlockStore for tests
- `internal/adapter/nfs/v4/handlers/helpers.go` - getBlockStoreForCtx returns *engine.BlockStore
- `internal/adapter/nfs/v4/handlers/read.go` - string conversions for ReadAt/ReadAtWithCOWSource
- `internal/adapter/nfs/v4/handlers/write.go` - string conversion for WriteAt
- `internal/adapter/nfs/v4/handlers/commit.go` - string conversion for Flush
- `internal/adapter/nfs/v4/handlers/io_test.go` - Constructs engine.BlockStore for tests
- `internal/adapter/smb/v2/handlers/close.go` - string conversions for Flush/ReadAt/Delete
- `internal/adapter/smb/v2/handlers/durable_scavenger.go` - Removed metadata.PayloadID cast
- `internal/adapter/smb/v2/handlers/flush.go` - string conversion for Flush
- `internal/adapter/smb/v2/handlers/handler.go` - string conversion for Flush
- `internal/adapter/smb/v2/handlers/read.go` - string conversion for ReadAt
- `internal/adapter/smb/v2/handlers/write.go` - string conversion for WriteAt
- `internal/controlplane/api/handlers/durable_handle.go` - Uses GetBlockStore, removes PayloadID cast

## Decisions Made

1. **engine/ sub-package instead of root blockstore package** - The orchestrator can't live in `pkg/blockstore/` because `blockstore/sync` and `blockstore/local` import `blockstore` root for types (HashSize, FileBlockStore, etc.), creating an import cycle. Placing it in `pkg/blockstore/engine/` breaks the cycle. The type is `engine.BlockStore` rather than `blockstore.BlockStore`.

2. **Local hashSize constant** - `pkg/blockstore/local/local.go` was importing `blockstore.HashSize` which created part of the cycle. Replaced with `const hashSize = 32` locally.

3. **string() conversion at adapter boundaries** - BlockStore methods take plain `string` for payloadID (matching blockstore.Store interface). Protocol adapters that use `metadata.PayloadID` (a named string type) need explicit `string()` conversion at each call site. This keeps the blockstore package free of metadata imports.

4. **Backward-compatible deprecation** - Kept `OffloaderConfig` as type alias for `SyncerConfig`, kept `GetPayloadService()`, `GetBlockService()`, `EnsurePayloadService()` as deprecated wrappers.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Import cycle forced engine/ sub-package**
- **Found during:** Task 1
- **Issue:** blockstore root package creates import cycle with sync/local sub-packages
- **Fix:** Moved orchestrator to pkg/blockstore/engine/ sub-package
- **Files modified:** pkg/blockstore/engine/engine.go (created), pkg/blockstore/local/local.go (removed import)
- **Verification:** go build ./... passes
- **Committed in:** 1dee4730

**2. [Rule 3 - Blocking] Protocol adapter compile errors after type change**
- **Found during:** Task 2
- **Issue:** All NFSv3/v4/SMB handlers and API handlers referenced *payload.PayloadService type and passed metadata.PayloadID to methods expecting string
- **Fix:** Updated return types, imports, and added string() conversions across 20 files
- **Files modified:** All protocol adapter handler files listed above
- **Verification:** go build ./... passes, go test ./pkg/controlplane/runtime/... passes, go vet ./... passes
- **Committed in:** 4ee59bf3

---

**Total deviations:** 2 auto-fixed (both Rule 3 - blocking)
**Impact on plan:** Both were necessary to make the project compile. The engine/ sub-package is a structural difference from the plan (which specified blockstore.BlockStore) but the functionality is identical. The adapter updates were wider than planned but required for correctness.

## Issues Encountered
- Import cycle between blockstore root and sub-packages required architectural workaround (engine/ sub-package)
- PayloadID type mismatch required string() conversions across all protocol adapter call sites (20+ files)

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- BlockStore orchestrator is fully wired and all protocol adapters use it
- Plan 04 (cleanup of old payload packages) can proceed -- old payload/ package is no longer imported by runtime or adapters
- The old payload.PayloadService, cache.BlockCache, and offloader.Offloader packages are now dead code ready for removal

## Self-Check: PASSED
- All 5 created/key files verified on disk
- Both task commits (1dee4730, 4ee59bf3) found in git history

---
*Phase: 45-package-restructure*
*Completed: 2025-03-09*
