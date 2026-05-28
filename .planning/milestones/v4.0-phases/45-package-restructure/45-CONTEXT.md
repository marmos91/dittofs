# Phase 45: Package Restructure - Context

**Gathered:** 2026-03-09
**Status:** Ready for planning

<domain>
## Phase Boundary

Reorganize storage code into a clean `pkg/blockstore/` hierarchy. Move `pkg/cache/` to `pkg/blockstore/local/fs/`, `pkg/payload/store/s3/` and `pkg/payload/store/memory/` to `pkg/blockstore/remote/`, rename offloader to sync, move GC, create orchestrator absorbing PayloadService, extract I/O into `pkg/blockstore/io/`, update all consumer imports, and delete old packages. This is pure restructuring with renames — no new features, no behavior changes.

</domain>

<decisions>
## Implementation Decisions

### RemoteStore Interface
- Keep same shape as current BlockStore (WriteBlock, ReadBlock, ReadBlockRange, DeleteBlock, DeleteByPrefix, ListByPrefix, Close, HealthCheck)
- Rename to RemoteStore, drop DirectWriteStore (already removed in Phase 42)
- S3 and memory implementations move unchanged

### LocalStore Interface
- Composed sub-interfaces: LocalReader, LocalWriter, LocalFlusher, LocalManager
- Composite LocalStore embeds all sub-interfaces
- Sub-interfaces exported (Phase 29: narrowest possible interfaces — NFS read handlers accept just LocalReader)
- All defined in single `local.go` file (sub-interfaces are small, 2-4 methods each)
- Both LocalStore and RemoteStore include Close() method
- fdCache stays internal to filesystem local store (pkg/blockstore/local/fs/)
- Full in-memory LocalStore implementation (pkg/blockstore/local/memory/) for tests — not a stub

### Block Types Location
- FileBlock, BlockState, block helpers move from pkg/metadata/object.go to pkg/blockstore/types.go
- FileBlockStore interface moves from pkg/metadata/store.go to pkg/blockstore/
- FileBlockStore conformance tests move from pkg/metadata/storetest/ to pkg/blockstore/storetest/
- Metadata store implementations import blockstore for the interface and implement it
- Single BlockSize constant in pkg/blockstore/ root — deduplicate from cache and payload/store
- FlushResult and StorageStats move to pkg/blockstore/ as blockstore.FlushResult and blockstore.Stats
- PayloadError renamed to BlockStoreError in pkg/blockstore/errors.go

### Offloader Rename Strategy (Deferred from Phase 41)
- Package renamed: pkg/payload/offloader/ -> pkg/blockstore/sync/
- Import alias when Go's sync collides: `blocksync`
- Main type: Syncer (constructor: sync.New())
- File renames: upload.go -> sync.go, download.go -> fetch.go
- Method renames: uploadPendingBlocks -> syncLocalBlocks, downloadBlock -> fetchBlock, processUploadResult -> processSyncResult
- Type renames: fileUploadState -> fileSyncState, downloadResult -> fetchResult, uploadResult -> syncResult
- Queue renames: TransferQueue -> SyncQueue, TransferQueueEntry -> SyncQueueEntry
- Log messages stay as-is (Sync:/Fetch: — already updated in Phase 41)
- dedup.go stays in sync/ package (tightly coupled to sync process)

### BlockStore Orchestrator
- Struct name: blockstore.BlockStore
- Constructor: blockstore.New(cfg blockstore.Config) -> options struct pattern
- Lifecycle: explicit Start(ctx) launches goroutines, Close() stops them (two-phase init)
- Recovery runs automatically during Start() before syncer goroutines launch
- Composition: local.LocalStore (interface) + remote.RemoteStore (interface, can be nil) + *sync.Syncer (concrete) + *gc.Collector (concrete)
- Implements blockstore.Store interface (composed sub-interfaces: Reader, Writer, Flusher)
- Stats() on the interface (accessible to API handlers and metrics without type assertions)
- Flush returns (*FlushResult, error) — same rich signature, renamed package

### I/O Package
- Create pkg/blockstore/io/ extracting read/write from PayloadService
- read.go + write.go with cache-aware I/O logic

### Move Sequencing
- Bottom-up: types/interfaces first, then implementations, then orchestrator, then consumers
- 3-4 plans, each compiles and passes tests independently
- Clean break per plan — no temporary aliases or re-exports (Phase 29 decision)
- One branch (gsd/phase-45-package-restructure), one atomic commit per plan

### Plan Boundaries
- Plan 1: Types + interfaces + conformance suites in new pkg/blockstore/ hierarchy
- Plan 2: Move implementations (local/fs, remote/s3, remote/memory, sync, gc)
- Plan 3: Orchestrator (blockstore.go) + io/ + config wiring + runtime wiring
- Plan 4: Consumer updates (NFS/SMB handlers, E2E tests) + old package deletion + docs

### Config and Runtime
- Config updates in Plan 3 with orchestrator (creates full BlockStore)
- Runtime wiring in Plan 3 (constructs BlockStore, special consumer)

### Testing Strategy
- Conformance suites: pkg/blockstore/local/localtest/ and pkg/blockstore/remote/remotetest/
- Both fs/ and memory/ local implementations must pass localtest/
- Both s3/ and memory/ remote implementations must pass remotetest/
- Existing cache_test.go moves to local/fs/ as implementation-specific tests + conformance suite
- Old integration test (blockstore_integration_test.go) replaced by conformance suites
- E2E test updates in this phase (Plan 4) — tests must compile after package moves

### Documentation
- All doc updates in final plan (Plan 4) — CLAUDE.md, ARCHITECTURE.md, CONFIGURATION.md
- doc.go per package (blockstore, local, remote, sync, gc, io, storetest)
- Drop pkg/payload/README.md — doc.go files are sufficient

### Claude's Discretion
- Exact sub-interface method groupings for LocalStore (which methods go in Reader vs Writer vs Flusher vs Manager)
- Exact sub-interface method groupings for blockstore.Store (Reader vs Writer vs Flusher)
- Conformance test structure and assertions
- doc.go content and wording
- Whether to split sync.go into smaller files within pkg/blockstore/sync/

</decisions>

<specifics>
## Specific Ideas

- Sync/fetch terminology everywhere — files, methods, types, logs all consistent
- User explicitly wants composed sub-interfaces at both levels (LocalStore and BlockStore orchestrator) following Phase 29 pattern
- Import alias for blockstore sync: `blocksync` (not `bsync`)
- FileBlockStore and block types should live with blockstore (their domain), not metadata (their current home)
- Recovery logic belongs in orchestrator (touches both local and remote state), not in local store

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- BlockStore interface (pkg/payload/store/store.go) — becomes RemoteStore with minimal changes
- BlockCache struct (pkg/cache/cache.go) — becomes local/fs/FSStore implementation
- Offloader (pkg/payload/offloader/) — becomes pkg/blockstore/sync/ with file/method/type renames
- GC (pkg/payload/gc/) — moves directly with minimal changes
- PayloadService (pkg/payload/service.go) — orchestration logic absorbed into blockstore.BlockStore
- FileBlock, BlockState, block helpers (pkg/metadata/object.go) — move to pkg/blockstore/types.go
- FileBlockStore interface (pkg/metadata/store.go) — moves to pkg/blockstore/
- FileBlockStore conformance tests (pkg/metadata/storetest/) — move to pkg/blockstore/storetest/

### Established Patterns
- Interface composition (Phase 29): narrowest interfaces, composite embeds all
- Conformance test suites (pkg/metadata/storetest/): shared tests all implementations must pass
- Clean break moves (Phase 29): no aliases, update all consumers in same PR
- In-memory implementations for tests (Phase 29): real implementations, not mocks

### Integration Points
- ~11 files import pkg/cache/ directly
- ~9 files import pkg/payload/
- ~75 files reference PayloadService/PayloadStore (includes E2E tests)
- pkg/config/stores.go creates stores via factory functions
- pkg/controlplane/runtime/init.go wires PayloadService
- NFS v3/v4 handlers and SMB handlers use PayloadService for I/O
- E2E test helpers create PayloadStore configurations

### Consumer Files to Update (~20+ Go files)
- internal/adapter/nfs/v3/handlers/ (read, write, create, commit, remove, utils, testing/fixtures)
- internal/adapter/nfs/v4/handlers/ (read, write, close, commit, helpers, io_test)
- pkg/controlplane/runtime/ (runtime.go, init.go, shares/service.go, runtime_test.go)
- pkg/config/ (stores.go, runtime.go, init.go)
- cmd/dfs/commands/start.go
- E2E test files (~30 files referencing PayloadStore)

</code_context>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 45-package-restructure*
*Context gathered: 2026-03-09*
