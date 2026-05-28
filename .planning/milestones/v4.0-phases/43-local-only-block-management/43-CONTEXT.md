# Phase 43: Local-Only Block Management - Context

**Gathered:** 2026-03-09
**Status:** Ready for planning

<domain>
## Phase Boundary

Add block management operations to the cache and support offloader without remote store (nil blockStore). This enables local-only mode where blocks are written to disk cache and stay in Local state — ready for future remote sync if a remote store is added later. No share model changes (Phase 44), no package restructure (Phase 45).

</domain>

<decisions>
## Implementation Decisions

### Nil-Store Error Semantics
- Offloader constructor `New()` accepts nil blockStore (no panic) — creates local-only offloader
- Each remote method (GetFileSize, Exists, Delete, Truncate) nil-guards internally: `if m.blockStore == nil { return 0/false/nil }`
- Debug log on nil-guard hits: `logger.Debug("offloader: skipping [op], no remote store")`
- Callers (PayloadService, NFS/SMB handlers) don't need to know about the mode — offloader handles it
- HealthCheck with nil blockStore returns nil (healthy, just no remote)
- Finalization callback (onFinalized) skipped entirely in local-only mode

### Local-Only Flush Behavior
- Flush still flushes memBlocks to .blk files on disk — disk IS the final store in local-only mode
- Blocks marked BlockStateLocal after flush (not Remote, not Finalized)
- Flush returns Finalized=false — blocks are in Local state, ready for future remote sync
- If remote store is added later, periodic syncer discovers existing Local blocks and uploads automatically — no migration needed

### Eviction Control
- `SetEvictionEnabled(enabled bool)` method on BlockCache (not offloader)
- In local-only mode, eviction is **hard-coded off** — SetEvictionEnabled(true) is a no-op when no remote store exists
- Blocks can't be evicted because they can't be re-fetched from remote
- Disk full behavior: same as current (ErrDiskFull after 30s backpressure wait). No special local-only handling
- When eviction is disabled, ensureSpace() skips ListRemoteBlocks query entirely (fast path, no wasted I/O)
- When eviction transitions from disabled to enabled (remote added), no immediate sweep — natural pressure-driven eviction
- No Prometheus metrics for disk usage in this phase (skip metrics)

### Periodic Syncer Lifecycle
- Don't start periodic syncer goroutine at all in local-only mode (nil blockStore)
- Add `SetRemoteStore(ctx context.Context, blockStore store.BlockStore)` method on offloader
- SetRemoteStore: sets blockStore, enables eviction on cache, starts periodic syncer — single method, atomic transition
- SetRemoteStore is one-shot only — errors if called when remote store already set
- SetRemoteStore accepts its own context for the syncer goroutine lifecycle
- When syncer starts after SetRemoteStore, waits for first periodic tick (2s) to discover Local blocks — no immediate scan

### Cache manage.go Methods
- New file: `pkg/cache/manage.go` with 5 management methods + SetEvictionEnabled
- All methods do **disk + metadata + memory** cleanup (complete, not partial):
  - `DeleteBlockFile(payloadID, blockIdx)` — purge memBlock + delete .blk file + remove FileBlock metadata + close FDs from fdCache/readFDCache
  - `DeleteAllBlockFiles(payloadID)` — delete all blocks for file + remove empty parent directory
  - `TruncateBlockFiles(payloadID, newSize)` — remove whole blocks where blockIdx * BlockSize >= newSize (no partial block truncation)
  - `GetStoredFileSize(payloadID)` — sum FileBlock.DataSize from metadata (no disk I/O, fast)
  - `ExistsOnDisk(payloadID, blockIdx)` — check FileBlock CachePath + os.Stat verification (handles stale metadata)
- All delete operations update diskUsed atomic counter
- All delete operations close open file descriptors from fdCache and readFDCache

### Remove() Rename
- Rename `Remove()` to `EvictMemory()` — clarifies it only releases in-memory blocks (handle close semantics)
- `EvictMemory()` = release memory on handle close (fast, keeps disk blocks)
- `DeleteAllBlockFiles()` = full cleanup on file deletion (memory + disk + metadata)
- Separate operations for separate concerns

### Code Organization
- `manage.go` = explicit block management operations (new file)
- `eviction.go` = automatic pressure-driven LRU eviction (stays separate)
- `manage_test.go` = dedicated test file for new methods
- Tests use existing in-memory FileBlockStore with nil blockStore — no new test infrastructure

### Runtime Wiring (LOCAL-04 Scope)
- Phase 43 scope: make offloader constructor accept nil blockStore, make init path able to pass nil
- Actual "create local-only share" flow deferred to Phase 44 (data model + API/CLI changes)
- --payload remains required in Phase 43; becomes --local (required) + --remote (optional) in Phase 44

### Claude's Discretion
- Exact nil-guard log message wording
- Whether SetEvictionEnabled needs a mutex guard or can use atomic bool
- Error message format for one-shot SetRemoteStore violation
- Test coverage depth for each manage.go method

</decisions>

<specifics>
## Specific Ideas

- User emphasized blocks should stay Local (not a new state) in local-only mode — enables natural auto-sync when remote is added later
- HealthCheck should move to BlockStore interface long-term (S3 → HeadBucket, local → Stat, memory → check) — captured for Phase 45
- User wants hot-add remote support now (SetRemoteStore) rather than deferring to a future phase

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `purgeMemBlocks(payloadID, shouldRemove)` in cache.go:331 — reusable for DeleteBlockFile/DeleteAllBlockFiles memory cleanup
- `evictBlock(ctx, fb)` in eviction.go:56 — pattern for disk deletion + metadata update (manage.go follows same pattern)
- `fdCache` and `readFDCache` in cache.go:63-67 — must be cleaned up in delete operations
- `transitionBlockState()` in cache.go:519 — used for state transitions, unchanged
- `recalcDiskUsed()` in eviction.go:87 — fallback for disk counter accuracy

### Established Patterns
- Offloader nil checks: already exists for blockStore in GetFileSize, Exists, etc. (lines 220, 258, 279, 311) — extend the pattern
- BlockCache atomic counters (memUsed, diskUsed) — manage.go must update diskUsed on delete
- Async FileBlock metadata via pendingFBs + SyncFileBlocks — manage.go should use direct PutFileBlock/DeleteFileBlock (not async) since these are explicit operations
- IsDirectWrite() early returns in offloader — similar pattern for nil blockStore

### Integration Points
- `pkg/payload/offloader/offloader.go:86` — New() constructor, remove blockStore nil panic
- `pkg/payload/offloader/offloader.go:327` — Start(), skip syncer if nil blockStore
- `pkg/payload/offloader/upload.go:28` — uploadPendingBlocks(), add nil blockStore guard
- `pkg/cache/cache.go:359` — Remove() rename to EvictMemory()
- `pkg/cache/eviction.go:16` — ensureSpace(), add eviction-disabled fast path
- All callers of cache.Remove() must update to EvictMemory()

</code_context>

<deferred>
## Deferred Ideas

- HealthCheck on BlockStore interface (S3 → HeadBucket, local FS → Stat, memory → check) — Phase 45 Package Restructure
- PayloadService elimination — Phase 45 (absorbed into pkg/blockstore/blockstore.go)
- Share model changes (--local required, --remote optional) — Phase 44 Data Model
- Disk usage Prometheus metrics for local-only mode — Phase 49 Testing & Docs

</deferred>

---

*Phase: 43-local-only-block-management*
*Context gathered: 2026-03-09*
