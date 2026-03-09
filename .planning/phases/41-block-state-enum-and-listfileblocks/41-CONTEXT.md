# Phase 41: Block State Enum and ListFileBlocks - Context

**Gathered:** 2026-03-09
**Status:** Ready for planning

<domain>
## Phase Boundary

Rename block state enum values and query methods to reflect the new two-tier Local/Remote terminology. Add ListFileBlocks query method. This is the foundational rename that all subsequent v4.0 phases build on. No new features, no architecture changes — pure terminology alignment.

</domain>

<decisions>
## Implementation Decisions

### State Enum Rename
- Full state enum: Dirty(0), Local(1), Syncing(2), Remote(3)
- `BlockStateSealed` → `BlockStateLocal`
- `BlockStateUploading` → `BlockStateSyncing` (bidirectional — covers both upload and download)
- `BlockStateUploaded` → `BlockStateRemote`
- State machine doc: `Dirty → Local → Syncing → Remote`
- `BlockStateDirty` stays as-is (value 0, zero-value safe default)

### Helper Method Renames (Full Rename)
- `IsSealed()` → `IsLocal()`
- `IsUploaded()` → `IsRemote()` (keep legacy migration fallback: Dirty + BlockStoreKey → treat as Remote)
- `MarkBlockUploaded()` → `MarkBlockRemote()`
- `MarkBlockUploading()` → `MarkBlockSyncing()`
- `MarkBlockPending()` → `MarkBlockLocal()`
- `WriteDownloaded()` → `WriteFromRemote()`

### Query Method Renames
- `ListPendingUpload()` → `ListLocalBlocks()` — interface + all 3 implementations (memory, badger, postgres) + transaction wrappers
- `ListEvictable()` → `ListRemoteBlocks()` — same scope

### ListFileBlocks Design
- Lives on `FileBlockStore` interface (natural home for block queries)
- Signature: `ListFileBlocks(ctx context.Context, payloadID string) ([]*FileBlock, error)`
- Claude's discretion: ordering (by block index) and whether to add optional state filter
- BadgerDB: new `fb-file:{payloadID}:{blockIdx}` secondary index
- Memory: iterate in-memory map filtered by payloadID
- PostgreSQL: `WHERE` clause on payloadID column (or join if needed)

### BadgerDB Migration
- Break compatibility — no migration code
- DittoFS is experimental; users clean DB on upgrade
- Secondary index prefix: `fb-sealed:` → `fb-local:`
- New secondary index: `fb-file:` for ListFileBlocks

### PostgreSQL Changes
- Keep `state` column as `SMALLINT` (no conversion to Postgres ENUM)
- Add inline SQL comments documenting state values (0=Dirty, 1=Local, 2=Syncing, 3=Remote)
- Rename partial indexes via migration:
  - `idx_file_blocks_pending` → `idx_file_blocks_local`
  - `idx_file_blocks_evictable` → `idx_file_blocks_remote`

### Documentation and Comments
- Full terminology update across all affected files
- State machine comments: "upload/download to block store" → "sync to/from remote"
- Cache comments, offloader comments, metadata comments all updated

### Log Messages
- Update ALL log messages referencing old terminology — both cache and offloader
- `uploadsReverted` → `syncsReverted`
- Offloader log messages: "Upload:" → "Sync:" or "Remote sync:", "Download:" → "Fetch:" or "Remote fetch:"
- Even though offloader method/file renames are deferred to Phase 45, log strings get updated now

### Test Updates
- Rename existing test functions to match new terminology (TestListPendingUpload → TestListLocalBlocks)
- Update assertion messages throughout
- Add conformance tests in `storetest/` for ListLocalBlocks, ListRemoteBlocks, and ListFileBlocks
- All 3 store implementations must pass conformance tests

### Recovery Logic
- `recovery.go` Syncing→Local revert stays (interrupted syncs get retried)
- Keep Dirty+BlockStoreKey→Remote fallback (defensive, handles partial recovery edge case)
- Rename variable: `uploadsReverted` → `syncsReverted`

### Scope Boundary — Deferred to Phase 45
- Offloader file renames (upload.go, download.go) — deferred to Phase 45 (Package Restructure)
- Offloader method renames (uploadPendingBlocks, downloadBlock, etc.) — deferred to Phase 45
- Offloader type renames (fileUploadState, downloadResult) — deferred to Phase 45
- Only offloader LOG STRINGS are updated in Phase 41

### Claude's Discretion
- ListFileBlocks: ordering (by block index likely), optional state filter parameter
- Exact conformance test structure and assertions
- Whether to add a `fb-file:` index in BadgerDB or use prefix scan on existing keys

</decisions>

<specifics>
## Specific Ideas

- "Syncing" chosen over "Uploading" because sync is bidirectional — covers both upload and download paths
- User emphasized breaking BadgerDB compat is fine — DittoFS is experimental
- User wants log messages updated across ALL packages now, not just the ones being renamed

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `transitionBlockState()` in `cache.go:519` — generic state transition method, just needs constant updates
- `parseBlockID()` in `recovery.go:125` — block ID parsing, unchanged
- BadgerDB secondary index pattern (`fb-sealed:` prefix + iteration) — reuse for `fb-local:` and `fb-file:`

### Established Patterns
- FileBlockStore interface in `pkg/metadata/store.go:208-253` — add ListFileBlocks here
- Each store implementation has matching transaction wrapper methods
- Postgres uses partial indexes with `WHERE state = N` for efficient queries
- BadgerDB uses secondary key prefixes for O(subset) iteration

### Integration Points
- `pkg/metadata/object.go` — BlockState enum and FileBlock helpers (primary change)
- `pkg/metadata/store.go` — FileBlockStore interface (add ListFileBlocks, rename queries)
- `pkg/metadata/store/{memory,badger,postgres}/objects.go` — 3 implementations + tx wrappers
- `pkg/cache/{cache,flush,write,recovery,eviction}.go` — cache methods using state constants
- `pkg/payload/offloader/{upload,download,dedup}.go` — offloader using state constants + log messages
- `pkg/metadata/storetest/` — conformance tests (add new tests)

### Files to Touch (~20 files)
- `pkg/metadata/object.go` — enum + helpers
- `pkg/metadata/store.go` — interface
- `pkg/metadata/store/memory/objects.go` — memory impl
- `pkg/metadata/store/badger/objects.go` — badger impl + indexes
- `pkg/metadata/store/postgres/objects.go` — postgres impl
- `pkg/metadata/store/postgres/migrations/` — new migration for index renames
- `pkg/cache/cache.go` — Mark* methods, comments
- `pkg/cache/flush.go` — state references, comments
- `pkg/cache/write.go` — state references, comments
- `pkg/cache/recovery.go` — recovery logic, log messages
- `pkg/cache/eviction.go` — ListEvictable→ListRemoteBlocks call
- `pkg/cache/read.go` — comments only
- `pkg/payload/offloader/upload.go` — state references + log messages
- `pkg/payload/offloader/download.go` — log messages
- `pkg/payload/offloader/dedup.go` — state references
- `pkg/payload/offloader/queue.go` — log messages
- `pkg/payload/offloader/types.go` — log messages
- `pkg/payload/offloader/offloader.go` — log messages
- `pkg/metadata/storetest/` — new conformance tests

</code_context>

<deferred>
## Deferred Ideas

- Offloader file/method/type renames (upload.go→sync_to_remote.go, etc.) — Phase 45 Package Restructure
- Converting Postgres state column to ENUM type — not needed, SMALLINT with comments is sufficient

</deferred>

---

*Phase: 41-block-state-enum-and-listfileblocks*
*Context gathered: 2026-03-09*
