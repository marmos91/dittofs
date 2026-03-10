# Phase 46: Per-Share Block Store Wiring - Context

**Gathered:** 2026-03-10
**Status:** Ready for planning

<domain>
## Phase Boundary

Replace the single global `*engine.BlockStore` on Runtime with per-share BlockStore instances. Each share gets its own BlockStore wired to its own local and (optional) remote block store configs from the control plane DB. NFS/SMB handlers resolve the correct BlockStore per request via file handle. Remove all global BlockStore infrastructure (EnsureBlockStore, GetBlockStore, SetBlockStore).

</domain>

<decisions>
## Implementation Decisions

### Block Store Registry Design
- Per-share BlockStore map keyed by **share name** (not share ID) — matches how handlers already resolve shares via GetShareNameForHandle
- **Eager creation** on AddShare — BlockStore created and started when share is loaded or created via API. Fails fast on invalid config
- **Close immediately** on RemoveShare — BlockStore.Close() drains uploads, closes local/remote stores. Clean resource release
- **Remove EnsureBlockStore entirely** — no global BlockStore path. Per-share is the only path. Clean break
- BlockStore field lives on `shares.Share` struct. `shares.Service` owns the map and lifecycle
- DrainAllUploads iterates all per-share BlockStores and drains each (single call from shutdown path)
- ShareConfig gets `LocalBlockStoreName` + `RemoteBlockStoreName` fields. AddShare resolves from DB, creates stores, wires BlockStore
- New `BlockStoreConfigProvider` interface (like MetadataStoreProvider) — AddShare accepts it to resolve config names from DB. Narrow interface, testable
- Hot-add: BlockStore created AND started immediately when share is created via API at runtime
- All shares' BlockStores created before any adapter starts serving on startup
- CacheConfig/SyncerConfig renamed (Claude's discretion on exact names) — remain global defaults for this phase, per-share tuning deferred to Phase 48

### Handler Resolution Path
- New `GetBlockStoreForHandle(handle)` method on Runtime — extracts share name from file handle, looks up BlockStore from shares.Service registry
- Returns `(nil, error)` when share has no BlockStore — handlers translate to NFS3ERR_IO / SMB STATUS_INTERNAL_ERROR
- NFSv3 existing helpers (`getBlockStoreOrReply`, `getBlockStoreForWriteOrReply` in utils.go) updated to accept file handle and call GetBlockStoreForHandle — minimal change to handler call sites
- SMB handlers: each call site updated to pass the file handle
- Health check endpoint: aggregates health from all per-share BlockStores. Reports healthy only if all pass. Shows which share(s) are degraded
- SMB durable handle scavenger: resolves BlockStore from the file handle stored in DurableState (same pattern as regular handlers)
- API durable_handle handler: resolves from handle in request

### Local Store Isolation
- Disk layout: `basePath/shares/{sanitizedShareName}/blocks/` — each share gets its own subdirectory
- basePath comes from the local BlockStoreConfig's JSON config field in DB — different local block store configs = different base paths
- Share name sanitized for filesystem safety (slashes replaced) — Claude's discretion on exact sanitization
- RemoveShare closes BlockStore but does **not delete** the local directory — admin can manually clean up or re-add share
- Each share's BlockStore uses the share's own metadata store instance as FileBlockStore — block metadata lives with file metadata
- Factory function `CreateLocalStoreFromConfig(ctx, storeType, cfg, shareName)` — parallels CreateRemoteStoreFromConfig. Respects type field: fs creates fs.FSStore, memory creates memory.MemoryLocalStore
- MaxPendingSize and MaxSize apply per-share from global defaults
- Share name uniqueness guaranteed at DB level (uniqueIndex) and runtime level (registry check) — no path collisions possible

### Shared Store Deduplication
- Remote stores shared: `map[remoteConfigName]remote.RemoteStore` cache in shares.Service. Multiple shares pointing to same S3 bucket reuse one S3 client
- Local stores always separate: each share gets its own local fs.FSStore regardless of local config
- Everything else separate per share: own local store, own syncer, own engine.BlockStore
- Reference counting on shared remote stores: when last share using a remote store is removed, close it and remove from cache

### Code Structure and Plans
- 3 plans:
  - Plan 1: shares.Service refactor (BlockStore field on Share, registry, factory interfaces, per-share creation/close, remote store cache with ref counting)
  - Plan 2: Handler updates (NFS v3/v4, SMB, API handlers switch to GetBlockStoreForHandle)
  - Plan 3: Cleanup (remove global EnsureBlockStore/GetBlockStore/SetBlockStore, rename CacheConfig/SyncerConfig, update tests + docs)
- Unit tests in this phase: two shares with different block store configs operating in isolation, verify independent read/write, verify BlockStore cleanup on RemoveShare
- Rewrite init_test.go for per-share (transform existing tests, don't delete)
- Update CLAUDE.md and ARCHITECTURE.md in Plan 3

### Claude's Discretion
- Exact rename for CacheConfig/SyncerConfig (e.g., LocalStoreDefaults, BlockStoreDefaults)
- Share name sanitization approach (replace slashes with underscores, URL encoding, hash, etc.)
- Exact BlockStoreConfigProvider interface method signatures
- Per-share MaxPendingSize default value calculation
- Factory function signatures and error handling details
- Remote store ref counting implementation details (atomic counter vs explicit tracking)
- Test structure and assertions

</decisions>

<specifics>
## Specific Ideas

- User wants the global EnsureBlockStore/GetBlockStore/SetBlockStore removed entirely — clean break consistent with Phase 44/45 approach
- User emphasized that share name uniqueness must be enforced (it already is: DB uniqueIndex + runtime check) — basePath/shares/{shareName}/blocks/ relies on this
- Remote store sharing is specifically for resource efficiency (single S3 client with connection pool) — not for sharing cache state
- User wants a `dfsctl share purge` command to clean up orphaned local store directories — captured as deferred idea

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `shares.Service` (pkg/controlplane/runtime/shares/service.go) — already manages share lifecycle, natural home for BlockStore registry
- `shares.Share` struct — gets new BlockStore field
- `shares.BlockStoreEnsurer` interface — to be replaced by BlockStoreConfigProvider
- `CreateRemoteStoreFromConfig` (init.go:249) — factory pattern to replicate for local stores
- `engine.BlockStore` (pkg/blockstore/engine/engine.go) — per-share orchestrator, no changes needed
- `shares.MetadataStoreProvider` interface — pattern to follow for BlockStoreConfigProvider
- `getBlockStoreOrReply` / `getBlockStoreForWriteOrReply` (internal/adapter/nfs/v3/handlers/utils.go) — helpers to update for per-share resolution

### Established Patterns
- Phase 29: Narrow interfaces (BlockStoreConfigProvider follows MetadataStoreProvider pattern)
- Phase 44: Clean break (remove old APIs entirely, no deprecation shims)
- Phase 45: Factory functions for store creation (CreateRemoteStoreFromConfig)
- Eager initialization on share load (metadata stores already work this way)
- Share name extracted from file handle via metadata.DecodeFileHandle

### Integration Points
- `pkg/controlplane/runtime/runtime.go` — remove global blockStore field, EnsureBlockStore, GetBlockStore, SetBlockStore
- `pkg/controlplane/runtime/init.go` — remove global EnsureBlockStore, add CreateLocalStoreFromConfig
- `pkg/controlplane/runtime/shares/service.go` — add BlockStore to Share, remote store cache, per-share wiring
- `internal/adapter/nfs/v3/handlers/utils.go` — update helpers for per-share resolution
- `internal/adapter/nfs/v4/handlers/helpers.go:105` — getBlockStore resolves per share
- `internal/adapter/smb/v2/handlers/*.go` — 6 files using GetBlockStore()
- `internal/controlplane/api/handlers/health.go` — aggregate health across all per-share BlockStores
- `internal/controlplane/api/handlers/durable_handle.go` — resolve per share from handle

### Consumer Files to Update (~20 files)
- NFS v3: utils.go, read.go, write.go, create.go, remove.go, commit.go, read_payload.go, testing/fixtures.go
- NFS v4: helpers.go, read.go, write.go, commit.go, io_test.go
- SMB: read.go, write.go, close.go, flush.go, handler.go, durable_scavenger.go
- API: health.go, durable_handle.go
- Runtime: runtime.go, init.go, init_test.go, runtime_test.go

</code_context>

<deferred>
## Deferred Ideas

- `dfsctl share purge` command — CLI command to clean up orphaned local store directories after share removal. Could also support `--all` to purge all local data. Future phase or backlog item.
- Per-share CacheConfig/SyncerConfig tuning — Phase 48 (auto-deduced config)
- Per-share MaxSize/MaxPendingSize override in DB config — Phase 48
- E2E tests for multi-share isolation — Phase 49

</deferred>

---

*Phase: 46-per-share-block-store-wiring*
*Context gathered: 2026-03-10*
