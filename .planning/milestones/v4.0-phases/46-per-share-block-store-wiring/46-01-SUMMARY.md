---
phase: 46-per-share-block-store-wiring
plan: 01
subsystem: runtime
tags: [blockstore, per-share, engine, ref-counting, lifecycle]

# Dependency graph
requires: []
provides:
  - "Per-share BlockStore lifecycle in shares.Service (AddShare/RemoveShare)"
  - "GetBlockStoreForHandle for file-handle to BlockStore resolution"
  - "Ref-counted shared remote store pool with nonClosingRemote wrapper"
  - "CreateLocalStoreFromConfig/CreateRemoteStoreFromConfig factory functions"
  - "DrainAllBlockStores iterating all per-share BlockStores"
  - "SetLocalStoreDefaults/SetSyncerDefaults on Runtime"
affects: [46-02, 46-03]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "nonClosingRemote wrapper to decouple engine.Close() from shared remote lifecycle"
    - "sharedRemote struct with refCount for shared remote store instances"
    - "BlockStoreConfigProvider interface for narrow DB access from shares package"
    - "sanitizeShareName for per-share directory naming"

key-files:
  created: []
  modified:
    - pkg/controlplane/runtime/shares/service.go
    - pkg/controlplane/runtime/runtime.go
    - pkg/controlplane/runtime/init.go
    - pkg/controlplane/runtime/init_test.go
    - pkg/controlplane/runtime/runtime_test.go
    - cmd/dfs/commands/start.go

key-decisions:
  - "nonClosingRemote wrapper prevents engine.BlockStore.Close() from closing shared remotes; actual close managed by ref counting in shares.Service"
  - "BlockStoreConfigProvider narrow interface avoids importing full store.Store into shares package"
  - "Backward compat: kept blockStore field, GetBlockStore(), SetBlockStore(), EnsureBlockStore() for Plan 03 removal"

patterns-established:
  - "nonClosingRemote: wrap shared resources so child Close() is a no-op; parent manages lifecycle via ref counting"
  - "BlockStoreConfigProvider: narrow interface for cross-package DB access"

requirements-completed: [SHARE-01, SHARE-02, SHARE-04]

# Metrics
duration: 45min
completed: 2026-03-10
---

# Phase 46 Plan 01: Per-Share Block Store Wiring Summary

**Per-share BlockStore lifecycle with ref-counted shared remote stores, nonClosingRemote wrapper, and full isolation test coverage**

## Performance

- **Duration:** ~45 min
- **Started:** 2026-03-10T10:45:00Z
- **Completed:** 2026-03-10T11:30:00Z
- **Tasks:** 4
- **Files modified:** 6

## Accomplishments
- Each share gets its own `*engine.BlockStore` created during `AddShare`, replacing the global singleton
- Shared remote stores use reference counting with `sharedRemote` struct; `nonClosingRemote` wrapper prevents `engine.Close()` from prematurely closing shared remotes
- `GetBlockStoreForHandle` resolves per-share BlockStore from file handle via share name extraction
- `DrainAllUploads` iterates all per-share BlockStores instead of draining a single global store
- `start.go` sets `LocalStoreDefaults` and `SyncerDefaults` before loading shares, removing `EnsureBlockStore` call
- 5 comprehensive tests: local-only, isolation, remote sharing with ref counting, close lifecycle, and handle resolution

## Task Commits

Each task was committed atomically:

1. **Task 0: Test skeletons (Wave 0 Nyquist)** - `792f4a31` (test)
2. **Task 1: Refactor shares.Service** - `86e62990` (feat)
3. **Task 2: GetBlockStoreForHandle + start.go** - `45f4a670` (feat)
4. **Task 3: Per-share isolation tests** - `d0b44a1e` (test)

## Files Created/Modified
- `pkg/controlplane/runtime/shares/service.go` - Added BlockStore field to Share, per-share lifecycle in AddShare/RemoveShare, sharedRemote pool with ref counting, nonClosingRemote wrapper, factory functions
- `pkg/controlplane/runtime/runtime.go` - Added GetBlockStoreForHandle, DrainAllUploads delegation, SetLocalStoreDefaults/SetSyncerDefaults
- `pkg/controlplane/runtime/init.go` - Updated LoadSharesFromStore to populate LocalBlockStoreID/RemoteBlockStoreID from DB model
- `pkg/controlplane/runtime/init_test.go` - 4 integration tests: local-only, isolation, remote sharing, close lifecycle
- `pkg/controlplane/runtime/runtime_test.go` - TestGetBlockStoreForHandle with handle resolution, error cases
- `cmd/dfs/commands/start.go` - Added SetLocalStoreDefaults/SetSyncerDefaults before LoadSharesFromStore, removed EnsureBlockStore

## Decisions Made
- **nonClosingRemote wrapper**: `engine.BlockStore.Close()` calls `remote.Close()` directly, which would close shared remote stores. The `nonClosingRemote` wrapper makes `Close()` a no-op; actual close is managed by `shares.Service.releaseRemoteStore` via ref counting.
- **BlockStoreConfigProvider interface**: Narrow interface (`GetBlockStoreByID`) avoids importing full `store.Store` into shares package, keeping dependencies minimal.
- **Backward compatibility preserved**: Kept `blockStore` field, `GetBlockStore()`, `SetBlockStore()`, `EnsureBlockStore()` on Runtime for Plan 03 removal. This avoids breaking other code that still references the global store.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] nonClosingRemote wrapper for shared remote stores**
- **Found during:** Task 3 (test implementation)
- **Issue:** `TestPerShareBlockStoreRemoteSharing` failed because `engine.BlockStore.Close()` closes the remote store directly, prematurely closing the shared remote when one share is removed while others still use it
- **Fix:** Added `nonClosingRemote` wrapper struct that embeds `remote.RemoteStore` and overrides `Close()` to be a no-op. Shared remotes are wrapped before passing to engine. The `shares.Service.releaseRemoteStore` handles actual close via ref counting.
- **Files modified:** `pkg/controlplane/runtime/shares/service.go`
- **Verification:** All 5 tests pass including remote sharing test with ref count assertions
- **Committed in:** `d0b44a1e` (Task 3 commit)

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Essential fix for correctness of shared remote store lifecycle. No scope creep.

## Issues Encountered
None beyond the auto-fixed deviation above.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Per-share BlockStore lifecycle is fully wired in `shares.Service`
- Plan 02 can now wire callers (NFS/SMB handlers) to use `GetBlockStoreForHandle` instead of `rt.GetBlockStore()`
- Plan 03 can remove the deprecated global `blockStore` field and `EnsureBlockStore` safely
- Backward compat shims remain in place for Plans 02/03

## Self-Check: PASSED

All 6 modified files verified present. All 4 task commit hashes verified in git log.

---
*Phase: 46-per-share-block-store-wiring*
*Completed: 2026-03-10*
