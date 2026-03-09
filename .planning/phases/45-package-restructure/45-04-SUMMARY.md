---
phase: 45-package-restructure
plan: 04
subsystem: blockstore
tags: [blockstore, cleanup, migration, payload-removal, cache-removal]

# Dependency graph
requires:
  - phase: 45-03
    provides: "engine.BlockStore orchestrator with all adapters wired"
provides:
  - "Complete removal of pkg/cache/ and pkg/payload/ directories"
  - "All consumers fully migrated to blockstore terminology"
  - "No deprecated payload/cache references in runtime or adapter code"
affects: [46, 47, 48, 49]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "BlockStoreEnsurer interface replacing PayloadServiceEnsurer"
    - "Direct method calls (EnsureBlockStore, SetSyncerConfig) instead of deprecated wrappers"

key-files:
  created: []
  modified:
    - cmd/dfs/commands/start.go
    - pkg/controlplane/runtime/runtime.go
    - pkg/controlplane/runtime/init.go
    - pkg/controlplane/runtime/runtime_test.go
    - pkg/controlplane/runtime/shares/service.go
    - pkg/adapter/adapter.go
    - pkg/adapter/errors.go
    - internal/adapter/nfs/v3/handlers/utils.go
    - internal/adapter/nfs/v3/handlers/testing/fixtures.go
    - internal/adapter/smb/v2/handlers/close.go
    - internal/adapter/smb/v2/handlers/durable_scavenger.go
    - internal/adapter/smb/v2/handlers/flush.go
    - internal/adapter/smb/v2/handlers/handler.go
    - internal/adapter/smb/v2/handlers/read.go
    - internal/adapter/smb/v2/handlers/write.go
    - internal/controlplane/api/handlers/health.go
  deleted:
    - pkg/cache/ (14 files)
    - pkg/payload/ (28 files)

key-decisions:
  - "Removed all deprecated aliases (GetPayloadService, GetBlockService, EnsurePayloadService, SetOffloaderConfig, OffloaderConfig) rather than keeping them"
  - "Renamed PayloadServiceEnsurer to BlockStoreEnsurer with method renames to match"
  - "Renamed ErrPayloadServiceNotInitialized to ErrBlockStoreNotInitialized"

patterns-established:
  - "BlockStoreEnsurer interface: EnsureBlockStore/HasBlockStore/HasStore for lazy block store init"

requirements-completed: [PKG-10, PKG-11]

# Metrics
duration: ~8min
completed: 2026-03-09
---

# Phase 45 Plan 04: Consumer Migration and Old Package Deletion Summary

**All consumer imports migrated to blockstore, deprecated aliases removed, pkg/cache/ and pkg/payload/ (42 files, 10,715 lines) deleted with clean build/test/vet**

## Performance

- **Duration:** ~8 min
- **Started:** 2026-03-09T20:00:26Z
- **Completed:** 2026-03-09T20:09:04Z
- **Tasks:** 2
- **Files modified:** 16 (+ 42 deleted)

## Accomplishments
- Migrated all remaining consumer references from payload/cache terminology to blockstore terminology
- Removed all deprecated backward-compatibility aliases from runtime (GetPayloadService, GetBlockService, EnsurePayloadService, SetOffloaderConfig, OffloaderConfig)
- Deleted pkg/cache/ (14 files) and pkg/payload/ (28 files) totaling 10,715 lines of dead code
- Full test suite, build, and vet pass cleanly after deletion

## Task Commits

Each task was committed atomically:

1. **Task 1: Update consumer imports and remove deprecated aliases** - `c36e3fde` (feat)
2. **Task 2: Delete old packages and verify clean build** - `d173621c` (feat)

## Files Created/Modified

### Modified (Task 1)
- `cmd/dfs/commands/start.go` - Calls EnsureBlockStore/SetSyncerConfig directly
- `pkg/controlplane/runtime/runtime.go` - Removed deprecated methods and OffloaderConfig alias
- `pkg/controlplane/runtime/init.go` - Removed EnsurePayloadService deprecated alias
- `pkg/controlplane/runtime/runtime_test.go` - Updated test for removed GetBlockService
- `pkg/controlplane/runtime/shares/service.go` - Renamed PayloadServiceEnsurer to BlockStoreEnsurer
- `pkg/adapter/adapter.go` - Updated comment reference to blockstore.ErrContentNotFound
- `pkg/adapter/errors.go` - Updated comment reference to blockstore.ErrContentNotFound
- `internal/adapter/nfs/v3/handlers/utils.go` - Renamed ErrPayloadServiceNotInitialized to ErrBlockStoreNotInitialized
- `internal/adapter/nfs/v3/handlers/testing/fixtures.go` - GetBlockStore instead of GetBlockService
- `internal/adapter/smb/v2/handlers/close.go` - GetBlockStore calls
- `internal/adapter/smb/v2/handlers/durable_scavenger.go` - GetBlockStore call
- `internal/adapter/smb/v2/handlers/flush.go` - GetBlockStore call
- `internal/adapter/smb/v2/handlers/handler.go` - GetBlockStore call
- `internal/adapter/smb/v2/handlers/read.go` - GetBlockStore call
- `internal/adapter/smb/v2/handlers/write.go` - GetBlockStore call
- `internal/controlplane/api/handlers/health.go` - GetBlockStore call, updated comment

### Deleted (Task 2)
- `pkg/cache/` - 14 files (BlockCache, WAL, eviction, read/write, recovery)
- `pkg/payload/` - 28 files (PayloadService, offloader, GC, store interface, memory/S3 stores)

## Decisions Made

1. **Removed deprecated aliases entirely** - Since Plan 03 already migrated all consumers, the deprecated methods (GetPayloadService, GetBlockService, EnsurePayloadService, SetOffloaderConfig) served no purpose. Removing them immediately rather than keeping them ensures no new code accidentally uses old paths.

2. **Renamed interface, not just methods** - PayloadServiceEnsurer became BlockStoreEnsurer with methods EnsureBlockStore/HasBlockStore to maintain naming consistency throughout the codebase.

3. **Renamed sentinel error** - ErrPayloadServiceNotInitialized became ErrBlockStoreNotInitialized for clarity in error messages and logs.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] GetBlockService references across SMB/NFS/API handlers**
- **Found during:** Task 1
- **Issue:** Plan 03 introduced GetBlockService as a deprecated alias but many SMB handlers, NFS test fixtures, and API health handler used it instead of GetBlockStore
- **Fix:** Updated all 8 files to use GetBlockStore directly
- **Files modified:** 6 SMB handler files, 1 NFS fixture, 1 API handler
- **Verification:** go build ./... passes
- **Committed in:** c36e3fde

---

**Total deviations:** 1 auto-fixed (Rule 1 - bug)
**Impact on plan:** Minor -- the GetBlockService calls needed updating as part of the cleanup. No scope creep.

## Issues Encountered
None - the migration was clean because Plan 03 had already done the heavy work of updating adapter code.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Phase 45 (Package Restructure) is now complete
- The entire storage hierarchy is unified under pkg/blockstore/
- No legacy pkg/cache/ or pkg/payload/ code remains
- All protocols (NFS v3, v4, SMB) use the new blockstore API
- Ready for Phase 46+ (NFSv4.2 features, etc.)

## Self-Check: PASSED
- All 5 key modified files verified on disk
- Both task commits (c36e3fde, d173621c) found in git history
- pkg/cache/ and pkg/payload/ directories confirmed deleted
- go build, go test, go vet all pass

---
*Phase: 45-package-restructure*
*Completed: 2026-03-09*
