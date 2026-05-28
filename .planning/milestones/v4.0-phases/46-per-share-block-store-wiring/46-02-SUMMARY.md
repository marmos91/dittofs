---
phase: 46-per-share-block-store-wiring
plan: 02
subsystem: nfs, smb, api
tags: [blockstore, per-share, nfs, smb, health, durable-handles]

# Dependency graph
requires:
  - phase: 46-01
    provides: GetBlockStoreForHandle, per-share BlockStore in Share struct, AddShare wiring
provides:
  - All NFS v3/v4 handlers resolve BlockStore per file handle
  - All SMB handlers resolve BlockStore per metadata handle
  - Health endpoint reports per-share block store status
  - Durable handle cleanup resolves BlockStore per handle
affects: [46-03-deprecate-global-block-store]

# Tech tracking
tech-stack:
  added: []
  patterns: [per-handle-block-store-resolution, per-share-health-aggregation]

key-files:
  created: []
  modified:
    - internal/adapter/nfs/v3/handlers/utils.go
    - internal/adapter/nfs/v3/handlers/read.go
    - internal/adapter/nfs/v3/handlers/write.go
    - internal/adapter/nfs/v3/handlers/create.go
    - internal/adapter/nfs/v3/handlers/remove.go
    - internal/adapter/nfs/v3/handlers/commit.go
    - internal/adapter/nfs/v3/handlers/doc.go
    - internal/adapter/nfs/v3/handlers/testing/fixtures.go
    - internal/adapter/nfs/v4/handlers/helpers.go
    - internal/adapter/nfs/v4/handlers/read.go
    - internal/adapter/nfs/v4/handlers/write.go
    - internal/adapter/nfs/v4/handlers/commit.go
    - internal/adapter/nfs/v4/handlers/io_test.go
    - internal/adapter/smb/v2/handlers/read.go
    - internal/adapter/smb/v2/handlers/write.go
    - internal/adapter/smb/v2/handlers/close.go
    - internal/adapter/smb/v2/handlers/flush.go
    - internal/adapter/smb/v2/handlers/handler.go
    - internal/adapter/smb/v2/handlers/durable_scavenger.go
    - internal/controlplane/api/handlers/health.go
    - internal/controlplane/api/handlers/durable_handle.go

key-decisions:
  - "Moved validation before handle resolution in NFS v3 write/create to preserve error code semantics"
  - "Changed health endpoint from single BlockStore to per-share block_stores array"
  - "Durable handle cleanup resolves BlockStore from MetadataHandle with graceful fallback"

patterns-established:
  - "Per-handle BlockStore resolution: all protocol handlers resolve via GetBlockStoreForHandle(ctx, handle)"
  - "Error ordering: validate request parameters before resolving per-share services to get correct error codes"

requirements-completed: [SHARE-03]

# Metrics
duration: 19min
completed: 2026-03-10
---

# Phase 46 Plan 02: Handler BlockStore Resolution Summary

**All NFS v3/v4 and SMB handler call sites migrated from global GetBlockStore() to per-share GetBlockStoreForHandle(ctx, handle) with per-share health endpoint aggregation**

## Performance

- **Duration:** 19 min (across sessions)
- **Started:** 2026-03-10T10:34:41Z
- **Completed:** 2026-03-10T10:52:59Z
- **Tasks:** 2/2
- **Files modified:** 21

## Accomplishments
- Migrated all 7 NFS v3 handler files to resolve BlockStore per file handle via getBlockStoreForHandle/getServicesForHandle helpers
- Migrated all 4 NFS v4 handler files to resolve BlockStore per compound context current file handle
- Migrated all 6 SMB handler files (read, write, close, flush, handler, durable_scavenger) to resolve BlockStore per open file metadata handle
- Updated health endpoint from single global BlockStore to per-share block_stores array
- Updated durable handle API cleanup to resolve BlockStore per metadata handle
- Updated test fixtures to inject BlockStore on share rather than global runtime

## Task Commits

Each task was committed atomically:

1. **Task 1: Update NFS v3 and v4 handler resolution to per-share** - `182c965b` (feat)
2. **Task 2: Update SMB handlers, API health, and durable handle resolution** - `8c270870` (feat)

## Files Created/Modified
- `internal/adapter/nfs/v3/handlers/utils.go` - Replaced getBlockStore/getServices with per-handle variants
- `internal/adapter/nfs/v3/handlers/read.go` - Resolve blockStore from fileHandle
- `internal/adapter/nfs/v3/handlers/write.go` - Validation before handle resolution, per-handle blockStore
- `internal/adapter/nfs/v3/handlers/create.go` - Per-handle resolution with NFS3ErrStale for bad handles
- `internal/adapter/nfs/v3/handlers/remove.go` - Per-handle resolution
- `internal/adapter/nfs/v3/handlers/commit.go` - Per-handle resolution
- `internal/adapter/nfs/v3/handlers/doc.go` - Updated checkMFsymlink call signature
- `internal/adapter/nfs/v3/handlers/testing/fixtures.go` - Inject BlockStore on share, not global
- `internal/adapter/nfs/v4/handlers/helpers.go` - New getBlockStoreForHandle replacing getBlockStoreForCtx
- `internal/adapter/nfs/v4/handlers/read.go` - Per-handle resolution via ctx.CurrentFH
- `internal/adapter/nfs/v4/handlers/write.go` - Per-handle resolution via ctx.CurrentFH
- `internal/adapter/nfs/v4/handlers/commit.go` - Per-handle resolution via ctx.CurrentFH
- `internal/adapter/nfs/v4/handlers/io_test.go` - Inject BlockStore on share
- `internal/adapter/smb/v2/handlers/read.go` - Per-handle resolution from openFile.MetadataHandle
- `internal/adapter/smb/v2/handlers/write.go` - Per-handle resolution from openFile.MetadataHandle
- `internal/adapter/smb/v2/handlers/close.go` - 3 call sites updated to per-handle resolution
- `internal/adapter/smb/v2/handlers/flush.go` - Per-handle resolution from openFile.MetadataHandle
- `internal/adapter/smb/v2/handlers/handler.go` - flushFileCache uses per-handle resolution
- `internal/adapter/smb/v2/handlers/durable_scavenger.go` - Resolve from PersistedDurableHandle.MetadataHandle
- `internal/controlplane/api/handlers/health.go` - Per-share block store health iteration
- `internal/controlplane/api/handlers/durable_handle.go` - Cleanup resolves from MetadataHandle

## Decisions Made
- **Validation before handle resolution (NFS v3):** Moved validateWriteRequest and related validation before getServicesForHandle/getBlockStoreForHandle calls. This ensures proper NFS error codes (NFS3ErrBadHandle) are returned for malformed handles instead of generic NFS3ErrIO from handle decoding failures.
- **NFS3ErrStale for unrecognizable handles in create.go:** Changed error return from NFS3ErrIO to NFS3ErrStale for handle resolution failures, matching test expectations and RFC 1813 semantics (stale file handle).
- **Health endpoint structural change:** Changed StoresResponse.BlockStore from `*StoreHealth` to `BlockStores []StoreHealth`, iterating per-share via ListShares()/GetShare(). This is a breaking API change for health consumers that parsed the old `block_store` field.
- **Graceful degradation for SMB close/scavenger:** Block store resolution errors in close and scavenger paths are logged but don't abort the operation, since these are cleanup paths where best-effort is appropriate.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed validation ordering in NFS v3 write handler**
- **Found during:** Task 1
- **Issue:** Per-handle resolution fails with NFS3ErrIO on malformed handles (empty, too short, too long) before validation can catch them with proper error codes
- **Fix:** Moved validateWriteRequest (Step 2) before handle resolution (Step 3)
- **Files modified:** internal/adapter/nfs/v3/handlers/write.go
- **Verification:** All NFS v3 handler tests pass
- **Committed in:** 182c965b (Task 1 commit)

**2. [Rule 1 - Bug] Fixed error status in NFS v3 create handler for bad handles**
- **Found during:** Task 1
- **Issue:** Handle resolution failure returned NFS3ErrIO but tests expected NFS3ErrStale for unrecognizable handles
- **Fix:** Changed error status from NFS3ErrIO to NFS3ErrStale for handle resolution failures
- **Files modified:** internal/adapter/nfs/v3/handlers/create.go
- **Verification:** TestCreate_HandleTooShort passes
- **Committed in:** 182c965b (Task 1 commit)

---

**Total deviations:** 2 auto-fixed (2 bug fixes)
**Impact on plan:** Both fixes necessary for correct error semantics. No scope creep.

## Issues Encountered
None beyond the deviations documented above.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- All handler call sites migrated from global GetBlockStore() to per-share resolution
- Plan 03 (deprecate global GetBlockStore) can safely remove the global method
- No remaining callers of GetBlockStore() in protocol handler paths

## Self-Check: PASSED

All commits verified (182c965b, 8c270870). All key files exist. Build, test, and vet pass clean.

---
*Phase: 46-per-share-block-store-wiring*
*Completed: 2026-03-10*
