# Phase 46: Per-Share Block Store Wiring - Research

**Researched:** 2026-03-10
**Domain:** Go runtime architecture / per-share storage isolation
**Confidence:** HIGH

## Summary

Phase 46 replaces the single global `*engine.BlockStore` on the Runtime with per-share BlockStore instances. Each share gets its own BlockStore wired to its own local and (optional) remote block store configs from the control plane DB. NFS/SMB handlers resolve the correct BlockStore per request via file handle. All global BlockStore infrastructure (EnsureBlockStore, GetBlockStore, SetBlockStore) is removed.

The codebase is well-prepared for this change. Phase 44 already added `LocalBlockStoreID` and `RemoteBlockStoreID` fields to the Share model. Phase 45 restructured packages into `pkg/blockstore/` with clean interfaces (`engine.BlockStore`, `local.LocalStore`, `remote.RemoteStore`, `sync.Syncer`). The existing `shares.Service` and file handle encoding (`shareName:uuid`) provide natural extension points for per-share storage routing.

**Primary recommendation:** Add a `*engine.BlockStore` field to `shares.Share`, make `shares.Service` own the per-share BlockStore lifecycle (creation on AddShare, close on RemoveShare), and add a `GetBlockStoreForHandle(handle)` method on Runtime that extracts share name from the file handle and delegates to shares.Service.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Per-share BlockStore map keyed by **share name** (not share ID) -- matches how handlers already resolve shares via GetShareNameForHandle
- **Eager creation** on AddShare -- BlockStore created and started when share is loaded or created via API. Fails fast on invalid config
- **Close immediately** on RemoveShare -- BlockStore.Close() drains uploads, closes local/remote stores. Clean resource release
- **Remove EnsureBlockStore entirely** -- no global BlockStore path. Per-share is the only path. Clean break
- BlockStore field lives on `shares.Share` struct. `shares.Service` owns the map and lifecycle
- DrainAllUploads iterates all per-share BlockStores and drains each (single call from shutdown path)
- ShareConfig gets `LocalBlockStoreName` + `RemoteBlockStoreName` fields. AddShare resolves from DB, creates stores, wires BlockStore
- New `BlockStoreConfigProvider` interface (like MetadataStoreProvider) -- AddShare accepts it to resolve config names from DB. Narrow interface, testable
- Hot-add: BlockStore created AND started immediately when share is created via API at runtime
- All shares' BlockStores created before any adapter starts serving on startup
- CacheConfig/SyncerConfig renamed (Claude's discretion on exact names) -- remain global defaults for this phase, per-share tuning deferred to Phase 48
- New `GetBlockStoreForHandle(handle)` method on Runtime -- extracts share name from file handle, looks up BlockStore from shares.Service registry
- Returns `(nil, error)` when share has no BlockStore -- handlers translate to NFS3ERR_IO / SMB STATUS_INTERNAL_ERROR
- NFSv3 existing helpers (`getBlockStoreOrReply`, `getBlockStoreForWriteOrReply` in utils.go) updated to accept file handle and call GetBlockStoreForHandle -- minimal change to handler call sites
- SMB handlers: each call site updated to pass the file handle
- Health check endpoint: aggregates health from all per-share BlockStores. Reports healthy only if all pass. Shows which share(s) are degraded
- SMB durable handle scavenger: resolves BlockStore from the file handle stored in DurableState (same pattern as regular handlers)
- API durable_handle handler: resolves from handle in request
- Disk layout: `basePath/shares/{sanitizedShareName}/blocks/` -- each share gets its own subdirectory
- basePath comes from the local BlockStoreConfig's JSON config field in DB -- different local block store configs = different base paths
- Share name sanitized for filesystem safety (slashes replaced) -- Claude's discretion on exact sanitization
- RemoveShare closes BlockStore but does **not delete** the local directory -- admin can manually clean up or re-add share
- Each share's BlockStore uses the share's own metadata store instance as FileBlockStore -- block metadata lives with file metadata
- Factory function `CreateLocalStoreFromConfig(ctx, storeType, cfg, shareName)` -- parallels CreateRemoteStoreFromConfig. Respects type field: fs creates fs.FSStore, memory creates memory.MemoryLocalStore
- MaxPendingSize and MaxSize apply per-share from global defaults
- Share name uniqueness guaranteed at DB level (uniqueIndex) and runtime level (registry check) -- no path collisions possible
- Remote stores shared: `map[remoteConfigName]remote.RemoteStore` cache in shares.Service. Multiple shares pointing to same S3 bucket reuse one S3 client
- Local stores always separate: each share gets its own local fs.FSStore regardless of local config
- Everything else separate per share: own local store, own syncer, own engine.BlockStore
- Reference counting on shared remote stores: when last share using a remote store is removed, close it and remove from cache
- 3 plans: Plan 1 (shares.Service refactor), Plan 2 (Handler updates), Plan 3 (Cleanup)
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

### Deferred Ideas (OUT OF SCOPE)
- `dfsctl share purge` command -- CLI command to clean up orphaned local store directories after share removal. Could also support `--all` to purge all local data. Future phase or backlog item.
- Per-share CacheConfig/SyncerConfig tuning -- Phase 48 (auto-deduced config)
- Per-share MaxSize/MaxPendingSize override in DB config -- Phase 48
- E2E tests for multi-share isolation -- Phase 49
</user_constraints>

<phase_requirements>

## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| SHARE-01 | Runtime manages per-share BlockStore instances (map[shareID]*BlockStore) replacing global PayloadService | BlockStore field on shares.Share, shares.Service owns lifecycle, remote store cache with ref counting. Research confirms shares.Service is the natural owner (already manages share lifecycle). |
| SHARE-02 | EnsureBlockStore(share) creates BlockStore with share's local + remote configs | Replaced by eager creation in AddShare. BlockStoreConfigProvider interface resolves configs from DB. CreateLocalStoreFromConfig factory parallels existing CreateRemoteStoreFromConfig pattern. |
| SHARE-03 | NFS/SMB handlers resolve BlockStore per share handle (getBlockStore(shareHandle)) | GetBlockStoreForHandle on Runtime extracts share name via metadata.DecodeFileHandle, delegates to shares.Service. Research confirms ~20 handler call sites across NFS v3/v4, SMB, API handlers. |
| SHARE-04 | Multiple shares with different local paths operate in isolation | Disk layout basePath/shares/{sanitizedShareName}/blocks/ ensures filesystem isolation. Each share gets own local.LocalStore, own syncer, own engine.BlockStore. Only remote stores are shared (with ref counting). |

</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `engine.BlockStore` | internal | Per-share orchestrator (local + remote + syncer) | Already exists from Phase 45. No changes needed to engine itself |
| `local.LocalStore` / `fs.FSStore` | internal | On-disk block cache per share | Existing interface. Each share gets its own instance |
| `remote.RemoteStore` | internal | Remote block storage (S3, memory) | Shared across shares referencing same config. Existing interface |
| `sync.Syncer` | internal | Async local-to-remote transfer | One per share. Existing implementation |
| `shares.Service` | internal | Share lifecycle + BlockStore registry | Natural home per CONTEXT.md decisions |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `blockstore.FileBlockStore` | internal | Block metadata storage | Each share's metadata store implements this. Already wired in Phase 45 |
| `metadata.DecodeFileHandle` | internal | Extract share name from NFS/SMB file handles | Used in GetBlockStoreForHandle to route to correct BlockStore |

No external dependencies needed. This is a pure internal refactoring phase.

## Architecture Patterns

### Recommended Changes to shares.Service

```
shares/
  service.go      # Add BlockStore field to Share, remote store cache, lifecycle
  testing.go      # Update InjectShareForTesting for BlockStore
```

### Pattern 1: BlockStore Field on Share Struct

**What:** Each `shares.Share` gets a `BlockStore *engine.BlockStore` field. `shares.Service` creates it during AddShare and closes it during RemoveShare.

**When to use:** Always -- this is the core pattern for per-share isolation.

**Current Share struct (service.go:14-40):**
```go
type Share struct {
    Name          string
    MetadataStore string
    RootHandle    metadata.FileHandle
    // ... existing fields ...
    // NEW:
    BlockStore    *engine.BlockStore // nil only for metadata-only shares (unlikely)
}
```

### Pattern 2: BlockStoreConfigProvider Interface

**What:** Narrow interface for resolving block store configs from DB by name. Follows `MetadataStoreProvider` pattern established in shares.Service.

**Example:**
```go
// BlockStoreConfigProvider resolves block store configurations from the control plane DB.
type BlockStoreConfigProvider interface {
    GetBlockStoreByID(ctx context.Context, id string) (*models.BlockStoreConfig, error)
}
```

Note: AddShare receives the block store config IDs via ShareConfig (from the Share model's LocalBlockStoreID/RemoteBlockStoreID). The provider resolves IDs to configs. This mirrors how metadata stores are resolved (metaStoreCfg looked up by ID in LoadSharesFromStore).

### Pattern 3: Remote Store Sharing with Reference Counting

**What:** `shares.Service` maintains `map[string]*sharedRemote` where each entry has a `remote.RemoteStore` and a reference count. Multiple shares pointing to the same remote config name reuse one remote store instance.

**When to use:** When multiple shares reference the same RemoteBlockStoreID.

**Example:**
```go
type sharedRemote struct {
    store    remote.RemoteStore
    refCount int
    configID string
}

type Service struct {
    // ... existing fields ...
    remoteStores map[string]*sharedRemote // configID -> shared remote
}
```

### Pattern 4: GetBlockStoreForHandle Resolution

**What:** Runtime method that extracts share name from file handle and returns the per-share BlockStore.

**Example:**
```go
func (r *Runtime) GetBlockStoreForHandle(ctx context.Context, handle metadata.FileHandle) (*engine.BlockStore, error) {
    shareName, err := r.sharesSvc.GetShareNameForHandle(ctx, handle)
    if err != nil {
        return nil, err
    }
    share, err := r.sharesSvc.GetShare(shareName)
    if err != nil {
        return nil, err
    }
    if share.BlockStore == nil {
        return nil, fmt.Errorf("share %q has no block store configured", shareName)
    }
    return share.BlockStore, nil
}
```

### Pattern 5: DrainAllUploads Iteration

**What:** On shutdown, iterate all shares and drain each BlockStore's uploads.

**Example:**
```go
func (r *Runtime) DrainAllUploads(ctx context.Context) error {
    return r.sharesSvc.DrainAllBlockStores(ctx)
}

// In shares.Service:
func (s *Service) DrainAllBlockStores(ctx context.Context) error {
    s.mu.RLock()
    blockStores := make([]*engine.BlockStore, 0, len(s.registry))
    for _, share := range s.registry {
        if share.BlockStore != nil {
            blockStores = append(blockStores, share.BlockStore)
        }
    }
    s.mu.RUnlock()

    var errs []error
    for _, bs := range blockStores {
        if err := bs.DrainAllUploads(ctx); err != nil {
            errs = append(errs, err)
        }
    }
    return errors.Join(errs...)
}
```

### Anti-Patterns to Avoid

- **Sharing local stores across shares:** Each share MUST have its own local.LocalStore. Local stores are tied to a specific directory and metadata store. Sharing would cause data corruption.
- **Lazy initialization of BlockStore:** The user explicitly decided on eager creation during AddShare. Don't add lazy init paths.
- **Keeping global EnsureBlockStore as fallback:** Clean break per CONTEXT.md. No deprecated wrappers.
- **Deleting local directories on RemoveShare:** The user explicitly decided against this. Local data persists for admin cleanup.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Remote store creation | Custom S3 factory | Existing `CreateRemoteStoreFromConfig` in init.go | Already handles s3/memory with proper config parsing |
| File handle decoding | Custom parser | `metadata.DecodeFileHandle(handle)` | Returns (shareName, uuid, err). Already used throughout |
| Block metadata storage | New store | Share's own MetadataStore (implements FileBlockStore) | Each metadata store already implements FileBlockStore |
| Syncer config | Per-share config parsing | Global `buildSyncerConfig()` with defaults | Per-share tuning deferred to Phase 48 |

**Key insight:** Most infrastructure already exists. This phase is about wiring existing components per-share instead of globally.

## Common Pitfalls

### Pitfall 1: Lock Ordering Between shares.Service.mu and BlockStore Operations
**What goes wrong:** Deadlock if BlockStore.Close() is called while holding shares.Service.mu, and a concurrent handler tries to acquire shares.Service.mu while holding a BlockStore internal lock.
**Why it happens:** BlockStore.Close() calls syncer.Close() which drains uploads, potentially blocking.
**How to avoid:** Copy the share/blockStore reference while holding mu, then release mu before calling Close(). Same pattern already used in notifyShareChange().
**Warning signs:** Tests hanging, deadlock detector in `go test -race`.

### Pitfall 2: Race Between RemoveShare and In-Flight Handler Requests
**What goes wrong:** Handler resolves BlockStore, RemoveShare closes it, handler calls ReadAt on closed BlockStore.
**Why it happens:** No coordination between handler request lifecycle and share removal.
**How to avoid:** BlockStore.Close() should be tolerant of post-close operations (return clean errors). The existing `closedFlag` in FSStore already handles this. For the MVP, this is acceptable -- production would need request draining.
**Warning signs:** "store closed" errors in logs after share removal.

### Pitfall 3: Forgetting to Update All Handler Call Sites
**What goes wrong:** Build breaks or runtime nil-pointer panics on untouched handlers.
**Why it happens:** ~20 files reference GetBlockStore(). Missing one causes a subtle runtime error.
**How to avoid:** Grep for ALL references: `GetBlockStore()` in handlers, then update each. The CONTEXT.md enumerates all files.
**Warning signs:** `go build ./...` fails, or `go vet ./...` catches unused imports.

### Pitfall 4: Share Name Sanitization Path Collisions
**What goes wrong:** Two share names map to the same filesystem path after sanitization.
**Why it happens:** Naive sanitization (e.g., `/export/foo` and `/export_foo` both become `export_foo`).
**How to avoid:** Use a deterministic reversible encoding. Recommended: replace `/` with `__` (double underscore). Share name uniqueness at DB level prevents collisions IF sanitization is injective.
**Warning signs:** Two shares writing to the same local directory.

### Pitfall 5: Not Starting BlockStore Before Adapters
**What goes wrong:** NFS/SMB handlers receive requests but BlockStore hasn't been started yet (background goroutines not running).
**Why it happens:** Incorrect startup ordering -- adapters start before all shares' BlockStores are started.
**How to avoid:** The existing startup flow already loads shares before loading adapters (start.go:143 then start.go:178). Per-share BlockStore.Start() happens during AddShare, which is called by LoadSharesFromStore, which runs before Serve().
**Warning signs:** "syncer not started" errors, blocks never uploaded.

### Pitfall 6: Remote Store Reference Count Going Negative
**What goes wrong:** Closing a remote store while another share still references it.
**Why it happens:** Bug in ref counting logic (double-decrement or race condition).
**How to avoid:** Protect refCount operations with shares.Service.mu (already held during AddShare/RemoveShare). Use simple int counter under the same lock.
**Warning signs:** "use of closed connection" errors from S3 client.

## Code Examples

### Current Handler Pattern (will change)
```go
// internal/adapter/nfs/v3/handlers/utils.go:60-66
func getBlockStore(reg *runtime.Runtime) (*engine.BlockStore, error) {
    blockStore := reg.GetBlockStore()
    if blockStore == nil {
        return nil, ErrBlockStoreNotInitialized
    }
    return blockStore, nil
}
```

### New Handler Pattern (target)
```go
// internal/adapter/nfs/v3/handlers/utils.go (updated)
func getBlockStoreForHandle(reg *runtime.Runtime, handle metadata.FileHandle) (*engine.BlockStore, error) {
    blockStore, err := reg.GetBlockStoreForHandle(context.Background(), handle)
    if err != nil {
        return nil, fmt.Errorf("block store not available: %w", err)
    }
    return blockStore, nil
}
```

### FSStore Creation for Share (new factory)
```go
// pkg/controlplane/runtime/init.go (new function)
func CreateLocalStoreFromConfig(
    ctx context.Context,
    storeType string,
    cfg interface{ GetConfig() (map[string]any, error) },
    shareName string,
    maxDisk int64,
    maxMemory int64,
    fileBlockStore blockstore.FileBlockStore,
) (local.LocalStore, error) {
    config, err := cfg.GetConfig()
    if err != nil {
        return nil, fmt.Errorf("failed to get config: %w", err)
    }

    switch storeType {
    case "fs":
        basePath, ok := config["path"].(string)
        if !ok || basePath == "" {
            return nil, errors.New("fs local store requires path")
        }
        sanitized := sanitizeShareName(shareName)
        cacheDir := filepath.Join(basePath, "shares", sanitized, "blocks")
        if err := os.MkdirAll(cacheDir, 0755); err != nil {
            return nil, fmt.Errorf("failed to create cache directory: %w", err)
        }
        return fs.New(cacheDir, maxDisk, maxMemory, fileBlockStore)
    case "memory":
        return memory.New(), nil
    default:
        return nil, fmt.Errorf("unsupported local store type: %s", storeType)
    }
}
```

### Share Name Sanitization
```go
func sanitizeShareName(name string) string {
    // Replace / with __ for filesystem safety.
    // Leading slash is common ("/export") -- trim it first.
    name = strings.TrimPrefix(name, "/")
    return strings.ReplaceAll(name, "/", "__")
}
```

### ShareConfig with Block Store Fields
```go
type ShareConfig struct {
    // ... existing fields ...
    LocalBlockStoreID  string // Required: resolved from Share model
    RemoteBlockStoreID string // Optional: resolved from Share model (empty = local-only)
}
```

### Health Check Aggregation
```go
// internal/controlplane/api/handlers/health.go (updated)
func (h *HealthHandler) Stores(w http.ResponseWriter, r *http.Request) {
    // ... metadata stores as before ...

    // Check per-share block stores
    blockStores := make([]StoreHealth, 0)
    for _, shareName := range h.registry.ListShares() {
        share, err := h.registry.GetShare(shareName)
        if err != nil || share.BlockStore == nil {
            continue
        }
        start := time.Now()
        err = share.BlockStore.HealthCheck(ctx)
        latency := time.Since(start)
        health := StoreHealth{
            Name:    fmt.Sprintf("block-store/%s", shareName),
            Type:    "block",
            Latency: latency.String(),
        }
        if err != nil {
            health.Status = "unhealthy"
            health.Error = err.Error()
            allHealthy = false
        } else {
            health.Status = "healthy"
        }
        blockStores = append(blockStores, health)
    }
    // ... return aggregated response ...
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Global `*engine.BlockStore` on Runtime | Per-share `*engine.BlockStore` on Share | Phase 46 (this phase) | All handlers must resolve per-handle |
| `EnsureBlockStore(ctx)` lazy global init | Eager per-share creation in AddShare | Phase 46 (this phase) | Startup fails fast on bad config |
| `GetBlockStore()` returns global | `GetBlockStoreForHandle(handle)` returns per-share | Phase 46 (this phase) | ~20 handler files updated |
| Single health check for one BlockStore | Aggregated health across all shares | Phase 46 (this phase) | Per-share degradation visible |

**Removed in this phase:**
- `Runtime.blockStore` field (global)
- `Runtime.EnsureBlockStore()` method
- `Runtime.GetBlockStore()` method
- `Runtime.SetBlockStore()` method
- `shares.BlockStoreEnsurer` interface
- `blockStoreHelper` struct in runtime.go

## Integration Points (Critical File Map)

### Plan 1: shares.Service Refactor
| File | Change |
|------|--------|
| `pkg/controlplane/runtime/shares/service.go` | Add BlockStore to Share, remote store cache, per-share creation/close |
| `pkg/controlplane/runtime/shares/testing.go` | Update InjectShareForTesting for BlockStore |
| `pkg/controlplane/runtime/runtime.go` | Add GetBlockStoreForHandle, update DrainAllUploads, update AddShare/RemoveShare |
| `pkg/controlplane/runtime/init.go` | Add CreateLocalStoreFromConfig, update LoadSharesFromStore to pass block store config IDs |
| `cmd/dfs/commands/start.go` | Remove EnsureBlockStore call, pass block store config info to LoadSharesFromStore |

### Plan 2: Handler Updates
| File | Current Pattern | New Pattern |
|------|----------------|-------------|
| `internal/adapter/nfs/v3/handlers/utils.go` | `getBlockStore(reg)` | `getBlockStoreForHandle(reg, handle)` |
| `internal/adapter/nfs/v3/handlers/read.go` | `getBlockStore(h.Registry)` | `getBlockStoreForHandle(h.Registry, handle)` |
| `internal/adapter/nfs/v3/handlers/commit.go` | `getBlockStore(h.Registry)` | `getBlockStoreForHandle(h.Registry, handle)` |
| `internal/adapter/nfs/v3/handlers/testing/fixtures.go` | `reg.GetBlockStore()` | Per-share (from share fixture) |
| `internal/adapter/nfs/v4/handlers/helpers.go` | `h.Registry.GetBlockStore()` | `h.Registry.GetBlockStoreForHandle(ctx, handle)` |
| `internal/adapter/nfs/v4/handlers/read.go` | Uses getBlockStoreForCtx | Updated to use handle |
| `internal/adapter/nfs/v4/handlers/write.go` | Uses getBlockStoreForCtx | Updated to use handle |
| `internal/adapter/nfs/v4/handlers/commit.go` | Uses getBlockStoreForCtx | Updated to use handle |
| `internal/adapter/smb/v2/handlers/read.go` | `h.Registry.GetBlockStore()` | Per-handle resolution |
| `internal/adapter/smb/v2/handlers/write.go` | `h.Registry.GetBlockStore()` | Per-handle resolution |
| `internal/adapter/smb/v2/handlers/close.go` (3 sites) | `h.Registry.GetBlockStore()` | Per-handle resolution |
| `internal/adapter/smb/v2/handlers/flush.go` | `h.Registry.GetBlockStore()` | Per-handle resolution |
| `internal/adapter/smb/v2/handlers/handler.go` | `h.Registry.GetBlockStore()` | Per-handle resolution |
| `internal/adapter/smb/v2/handlers/durable_scavenger.go` | `s.handler.Registry.GetBlockStore()` | Resolve from DurableState handle |
| `internal/controlplane/api/handlers/health.go` | `h.registry.GetBlockStore()` | Iterate per-share |
| `internal/controlplane/api/handlers/durable_handle.go` | `rt.GetBlockStore()` | Resolve from handle in request |

### Plan 3: Cleanup
| File | Change |
|------|--------|
| `pkg/controlplane/runtime/runtime.go` | Remove blockStore field, SetBlockStore, GetBlockStore, EnsureBlockStore, SetCacheConfig, GetCacheConfig, SetSyncerConfig, blockStoreHelper, CacheConfig, SyncerConfig types (rename and move) |
| `pkg/controlplane/runtime/init.go` | Remove EnsureBlockStore method, remove buildSyncerConfig (move to shares) |
| `pkg/controlplane/runtime/init_test.go` | Rewrite TestEnsureBlockStoreLocalOnly for per-share |
| `pkg/controlplane/runtime/runtime_test.go` | Remove TestGetServices block store nil check, add per-share tests |
| docs/ARCHITECTURE.md | Update diagrams |
| CLAUDE.md | Update package structure documentation |

## Naming Recommendations (Claude's Discretion)

### CacheConfig/SyncerConfig Rename
Recommendation: Rename to `LocalStoreDefaults` and `SyncerDefaults`.

Rationale: These are global defaults applied to every share's BlockStore. "Defaults" communicates that per-share override is planned (Phase 48). "LocalStore" aligns with the `local.LocalStore` interface name.

```go
type LocalStoreDefaults struct {
    BasePath       string // Base directory for share local store directories
    MaxSize        uint64 // Maximum local store size per share (0 = unlimited)
    MaxPendingSize uint64 // Maximum pending data per share (0 = default 2GB)
    MaxMemory      int64  // Memory budget for dirty buffers per share (0 = 256MB)
}

type SyncerDefaults struct {
    ParallelUploads    int
    ParallelDownloads  int
    PrefetchBlocks     int
    SmallFileThreshold int64
    UploadInterval     time.Duration
    UploadDelay        time.Duration
}
```

### Share Name Sanitization
Recommendation: Trim leading `/`, replace remaining `/` with `__` (double underscore).

Rationale: Simple, human-readable, reversible. Share names are typically short paths like `/export`, `/temp`, `/archive/cold`. Double underscore is extremely unlikely in real share names and clearly signals a separator. Examples:
- `/export` -> `export`
- `/archive/cold` -> `archive__cold`
- `/data` -> `data`

### BlockStoreConfigProvider Interface
Recommendation: Accept `store.BlockStoreConfigStore` sub-interface directly (it's already narrow).

The existing `store.BlockStoreConfigStore` interface from `pkg/controlplane/store/interface.go` has exactly the method we need: `GetBlockStoreByID(ctx, id)`. Rather than creating yet another interface, accept this existing narrow interface.

However, if we want maximum testability, define a local interface:
```go
type BlockStoreConfigProvider interface {
    GetBlockStoreByID(ctx context.Context, id string) (*models.BlockStoreConfig, error)
}
```

This is satisfied by the existing `store.BlockStoreConfigStore` without changes.

### Remote Store Ref Counting
Recommendation: Simple int counter protected by `shares.Service.mu`.

Rationale: AddShare and RemoveShare both already hold `shares.Service.mu`. No need for atomic operations or separate locks. The counter is only modified during share add/remove, which are admin operations (not hot path).

## Open Questions

1. **SMB handler file handle availability**
   - What we know: SMB handlers have access to the open file's metadata handle (e.g., in `OpenFile.MetadataHandle`). The metadata handle encodes the share name.
   - What's unclear: Need to verify each SMB handler call site has the handle readily available or if we need to extract it from the open file state.
   - Recommendation: Verify during Plan 2 implementation. The handle is almost certainly available in the session/open file context.

2. **Startup ordering for hot-added shares**
   - What we know: shares.Service.AddShare creates and starts BlockStore. The API handler for share creation calls runtime.AddShare.
   - What's unclear: Whether the API handler needs additional synchronization to ensure BlockStore is fully started before returning success to the client.
   - Recommendation: BlockStore.Start() is synchronous (recovery + goroutine launch). No additional sync needed.

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go testing (stdlib) |
| Config file | none (Go convention) |
| Quick run command | `go test ./pkg/controlplane/runtime/...` |
| Full suite command | `go test ./pkg/controlplane/... ./internal/adapter/... ./cmd/...` |

### Phase Requirements -> Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| SHARE-01 | Runtime manages per-share BlockStore instances | unit | `go test ./pkg/controlplane/runtime/ -run TestPerShareBlockStore -v` | Wave 0 |
| SHARE-02 | AddShare creates BlockStore with share's configs | unit | `go test ./pkg/controlplane/runtime/ -run TestAddShareCreatesBlockStore -v` | Wave 0 |
| SHARE-03 | GetBlockStoreForHandle resolves per share | unit | `go test ./pkg/controlplane/runtime/ -run TestGetBlockStoreForHandle -v` | Wave 0 |
| SHARE-04 | Multiple shares with different paths isolated | unit | `go test ./pkg/controlplane/runtime/ -run TestMultiShareIsolation -v` | Wave 0 |

### Sampling Rate
- **Per task commit:** `go build ./... && go test ./pkg/controlplane/runtime/...`
- **Per wave merge:** `go build ./... && go test ./...`
- **Phase gate:** Full suite green before `/gsd:verify-work`

### Wave 0 Gaps
- None -- existing test infrastructure covers all phase requirements. init_test.go and runtime_test.go already exist and will be transformed.

## Sources

### Primary (HIGH confidence)
- Codebase analysis of all referenced files (runtime.go, shares/service.go, init.go, engine/engine.go, handlers/utils.go, etc.)
- CONTEXT.md decisions document (46-CONTEXT.md)
- REQUIREMENTS.md phase requirement definitions

### Secondary (MEDIUM confidence)
- Startup flow analysis (cmd/dfs/commands/start.go)
- Handler call site enumeration via grep

### Tertiary (LOW confidence)
- None. All findings are from direct codebase inspection.

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - all components already exist, this is wiring/plumbing
- Architecture: HIGH - patterns are established in prior phases (MetadataStoreProvider, CreateRemoteStoreFromConfig)
- Pitfalls: HIGH - identified from direct code analysis of lock patterns, startup ordering, and race conditions

**Research date:** 2026-03-10
**Valid until:** Indefinite (internal codebase, not external dependencies)
