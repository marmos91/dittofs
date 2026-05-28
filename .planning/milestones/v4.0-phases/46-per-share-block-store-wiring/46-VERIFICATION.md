---
phase: 46-per-share-block-store-wiring
verified: 2026-03-10T11:11:33Z
status: passed
score: 5/5 truths verified
re_verification: false
---

# Phase 46: Per-Share Block Store Wiring Verification Report

**Phase Goal:** Wire per-share BlockStore through shares.Service so each share owns its own local cache + syncer + optional remote, replacing the single global BlockStore.

**Verified:** 2026-03-10T11:11:33Z

**Status:** passed

**Re-verification:** No - initial verification

## Goal Achievement

### Observable Truths

| # | Truth | Status | Evidence |
|---|-------|--------|----------|
| 1 | Runtime manages map[shareID]*BlockStore instead of single PayloadService | ✓ VERIFIED | shares.Service.registry map contains Share structs with BlockStore field; no global blockStore field on Runtime |
| 2 | EnsureBlockStore(share) creates BlockStore with share's local + remote configs | ✓ VERIFIED | AddShare creates BlockStore from LocalBlockStoreID/RemoteBlockStoreID; factory functions CreateLocalStoreFromConfig and acquireRemoteStore resolve configs |
| 3 | NFS/SMB handlers resolve BlockStore per share handle via getBlockStore(shareHandle) | ✓ VERIFIED | NFS v3 uses getBlockStoreForHandle(reg, handle), NFS v4 uses getBlockStoreForHandle(h, ctx.CurrentFH), SMB uses GetBlockStoreForHandle(ctx, openFile.MetadataHandle) |
| 4 | Multiple shares with different local paths operate in isolation | ✓ VERIFIED | TestPerShareBlockStoreIsolation verifies two shares with different local stores write/read independently; CreateLocalStoreFromConfig creates per-share directory under basePath/shares/{sanitizedName}/blocks/ |
| 5 | Share deletion cleans up associated BlockStore | ✓ VERIFIED | RemoveShare closes BlockStore and decrements remote ref count via releaseRemoteStore |

**Score:** 5/5 truths verified

### Required Artifacts

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| pkg/controlplane/runtime/shares/service.go | BlockStore field on Share, remote store cache, per-share lifecycle | ✓ VERIFIED | Share.BlockStore field exists (line 57), sharedRemote struct with refCount, AddShare creates BlockStore (lines 329-370), RemoveShare closes it |
| pkg/controlplane/runtime/runtime.go | GetBlockStoreForHandle, DrainAllUploads delegation | ✓ VERIFIED | GetBlockStoreForHandle exists (line 226), delegates to sharesSvc; DrainAllUploads delegates to sharesSvc.DrainAllBlockStores (line 318) |
| pkg/controlplane/runtime/init.go | CreateLocalStoreFromConfig factory | ✓ VERIFIED | Function exists in shares/service.go (line 633), creates per-share directory with sanitizeShareName |
| pkg/controlplane/runtime/init_test.go | Per-share BlockStore tests | ✓ VERIFIED | TestPerShareBlockStoreLocalOnly, TestPerShareBlockStoreIsolation, TestPerShareBlockStoreRemoteSharing, TestRemoveShareClosesBlockStore all exist and pass |
| internal/adapter/nfs/v3/handlers/utils.go | getBlockStoreForHandle helper | ✓ VERIFIED | Function exists (line 63), calls reg.GetBlockStoreForHandle |
| internal/adapter/nfs/v4/handlers/helpers.go | Updated getBlockStoreForCtx | ✓ VERIFIED | getBlockStoreForHandle exists, accepts handle parameter, calls GetBlockStoreForHandle |
| internal/controlplane/api/handlers/health.go | Per-share health check aggregation | ✓ VERIFIED | Iterates ListShares() (line 175), checks each share's BlockStore, aggregates into BlockStores array |
| CLAUDE.md | Updated architecture documentation | ✓ VERIFIED | 10 occurrences of "per-share", GetBlockStoreForHandle documented, architecture diagram updated, no references to global BlockStore |

### Key Link Verification

| From | To | Via | Status | Details |
|------|----|----|--------|---------|
| pkg/controlplane/runtime/shares/service.go | pkg/blockstore/engine/engine.go | AddShare creates engine.BlockStore per share | ✓ WIRED | engine.New called at line 363 with per-share config |
| pkg/controlplane/runtime/runtime.go | pkg/controlplane/runtime/shares/service.go | GetBlockStoreForHandle delegates to shares.Service | ✓ WIRED | Calls sharesSvc.GetShare at line 231 to retrieve share's BlockStore |
| pkg/controlplane/runtime/shares/service.go | pkg/blockstore/local/fs/ | CreateLocalStoreFromConfig creates fs.FSStore per share | ✓ WIRED | fs.New called at line 664 with per-share cacheDir |
| cmd/dfs/commands/start.go | pkg/controlplane/runtime/init.go | LoadSharesFromStore receives config defaults | ✓ WIRED | SetLocalStoreDefaults/SetSyncerDefaults called before LoadSharesFromStore |
| internal/adapter/nfs/v3/handlers/utils.go | pkg/controlplane/runtime/runtime.go | getBlockStoreForHandle calls reg.GetBlockStoreForHandle | ✓ WIRED | Direct call at line 63 |
| internal/adapter/smb/v2/handlers/read.go | pkg/controlplane/runtime/runtime.go | h.Registry.GetBlockStoreForHandle | ✓ WIRED | Called at line 191 with openFile.MetadataHandle |
| internal/controlplane/api/handlers/health.go | pkg/controlplane/runtime/runtime.go | Iterates shares, checks each BlockStore | ✓ WIRED | ListShares() at line 175, GetShare() retrieves share with BlockStore |

### Requirements Coverage

| Requirement | Source Plan | Description | Status | Evidence |
|-------------|-------------|-------------|--------|----------|
| SHARE-01 | 46-01, 46-03 | Runtime manages per-share BlockStore instances (map[shareID]*BlockStore) replacing global PayloadService | ✓ SATISFIED | shares.Service.registry contains Share structs with BlockStore field; global blockStore field removed from Runtime |
| SHARE-02 | 46-01, 46-03 | EnsureBlockStore(share) creates BlockStore with share's local + remote configs | ✓ SATISFIED | AddShare creates BlockStore from LocalBlockStoreID/RemoteBlockStoreID; factory functions resolve configs; old EnsureBlockStore removed |
| SHARE-03 | 46-02, 46-03 | NFS/SMB handlers resolve BlockStore per share handle (getBlockStore(shareHandle)) | ✓ SATISFIED | All 21 handler files updated to use GetBlockStoreForHandle with file/metadata handle |
| SHARE-04 | 46-01, 46-03 | Multiple shares with different local paths operate in isolation | ✓ SATISFIED | TestPerShareBlockStoreIsolation verifies isolation; CreateLocalStoreFromConfig creates per-share directories |

### Anti-Patterns Found

None. Clean implementation with no blockers or warnings.

### Human Verification Required

None. All functionality is programmatically verifiable and has passing test coverage.

## Verification Details

### Plan 01: Per-Share BlockStore Lifecycle

**Verified artifacts:**
- Share struct has BlockStore field (pkg/controlplane/runtime/shares/service.go:57)
- AddShare creates BlockStore from config IDs with eager initialization (lines 329-370)
- RemoveShare closes BlockStore and decrements remote ref count (lines 412-434)
- Remote store cache with reference counting (sharedRemote struct, acquireRemoteStore/releaseRemoteStore methods)
- nonClosingRemote wrapper prevents premature closure of shared remotes
- CreateLocalStoreFromConfig factory creates per-share directory under basePath/shares/{sanitizedName}/blocks/ (lines 633-669)
- sanitizeShareName trims leading / and replaces / with __ (lines 671-674)
- DrainAllBlockStores iterates all per-share BlockStores (lines 612-628)

**Verified tests:**
- TestPerShareBlockStoreLocalOnly: Single share gets working BlockStore
- TestPerShareBlockStoreIsolation: Two shares with different local stores operate independently
- TestPerShareBlockStoreRemoteSharing: Two shares reuse same remote store with ref counting
- TestRemoveShareClosesBlockStore: Removed share's BlockStore is closed
- TestGetBlockStoreForHandle: Resolves per-share BlockStore from file handle

All tests pass (verified 2026-03-10).

### Plan 02: Handler BlockStore Resolution

**Verified artifacts:**
- NFS v3 handlers: getBlockStoreForHandle and getServicesForHandle helpers (internal/adapter/nfs/v3/handlers/utils.go:63, 73)
- NFS v3 handlers updated: read.go, write.go, create.go, remove.go, commit.go all use per-handle resolution
- NFS v4 handlers: getBlockStoreForHandle accepts compound context file handle (internal/adapter/nfs/v4/handlers/helpers.go)
- NFS v4 handlers updated: read.go, write.go, commit.go use ctx.CurrentFH
- SMB handlers: All 6 files (read, write, close, flush, handler, durable_scavenger) resolve from openFile.MetadataHandle
- Health endpoint: Iterates ListShares(), checks each share's BlockStore, returns BlockStores array
- Durable handle cleanup: Resolves from MetadataHandle in both API and scavenger paths

**Build verification:** go build ./... passes with no errors

### Plan 03: Remove Global BlockStore Infrastructure

**Verified removals:**
- Runtime.blockStore field: REMOVED (grep confirms zero references)
- blockStoreHelper struct: REMOVED (grep confirms zero references)
- GetBlockStore() method: REMOVED (only GetBlockStoreForHandle exists)
- SetBlockStore() method: REMOVED
- EnsureBlockStore() method: REMOVED from both runtime.go and init.go
- CacheConfig/SyncerConfig old types: REMOVED (only LocalStoreDefaults/SyncerDefaults remain)
- CreateRemoteStoreFromConfig: REMOVED from init.go (shares.Service has its own copy)

**Verified documentation:**
- CLAUDE.md updated with 10 occurrences of "per-share"
- Architecture diagram shows per-share BlockStore flow
- Key Interfaces section documents GetBlockStoreForHandle
- Directory Structure updated
- Write Coordination Pattern updated to use per-share BlockStore
- No references to global BlockStore or PayloadService

**Grep verification (zero results):**
```bash
# All commands returned zero results:
grep -rn "func.*GetBlockStore()" pkg/ --include="*.go" | grep -v "ForHandle" | grep -v "_test.go"
grep -rn "EnsureBlockStore" pkg/ --include="*.go" | grep -v "_test.go"
grep -rn "blockStoreHelper" pkg/ --include="*.go"
grep -rn "BlockStoreEnsurer" pkg/ --include="*.go"
```

## Summary

Phase 46 goal **fully achieved**. All 5 success criteria from ROADMAP.md verified:

1. ✓ Runtime manages map[shareID]*BlockStore instead of single PayloadService
2. ✓ EnsureBlockStore(share) creates BlockStore with share's local + remote configs
3. ✓ NFS/SMB handlers resolve BlockStore per share handle via getBlockStore(shareHandle)
4. ✓ Multiple shares with different local paths operate in isolation
5. ✓ Share deletion cleans up associated BlockStore

All 4 requirements (SHARE-01 through SHARE-04) satisfied with implementation evidence and passing tests. No gaps, no regressions, no deferred work.

**Key accomplishments:**
- Per-share BlockStore lifecycle fully wired in shares.Service
- 21 handler files migrated from global GetBlockStore() to per-handle resolution
- Remote store sharing with reference counting prevents resource leaks
- nonClosingRemote wrapper prevents premature closure of shared remotes
- Comprehensive test coverage including isolation, remote sharing, and cleanup
- Clean removal of all global BlockStore infrastructure (279 lines deleted)
- Documentation fully updated to reflect per-share architecture

**Verification commands executed:**
- go build ./... - PASSED
- go test ./pkg/controlplane/runtime/... - PASSED (all 5 per-share tests)
- grep verification for removed infrastructure - PASSED (zero references)

---

_Verified: 2026-03-10T11:11:33Z_
_Verifier: Claude (gsd-verifier)_
