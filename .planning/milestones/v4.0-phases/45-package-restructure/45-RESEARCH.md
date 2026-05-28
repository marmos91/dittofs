# Phase 45: Package Restructure - Research

**Researched:** 2026-03-09
**Domain:** Go package reorganization, large-scale refactoring
**Confidence:** HIGH

## Summary

Phase 45 is a pure restructuring phase that moves existing code from `pkg/cache/`, `pkg/payload/`, and parts of `pkg/metadata/` into a clean `pkg/blockstore/` hierarchy. No new features or behavior changes are introduced. The primary challenge is managing the dependency graph during the move to ensure each plan compiles and passes tests independently, while handling the ~23 consumer files that import the affected packages.

The existing code is well-structured with clear interfaces (BlockStore, BlockCache, Offloader, GC) and the CONTEXT.md provides highly specific decisions on naming, interface composition, and plan boundaries. The research confirms all moves are feasible with the bottom-up sequencing decided: types/interfaces first, then implementations, then orchestrator, then consumers + deletion.

**Primary recommendation:** Follow the 4-plan bottom-up strategy exactly as specified in CONTEXT.md. Each plan must compile and pass `go build ./...` and `go test ./...` independently. The critical ordering constraint is that types and interfaces (Plan 1) must land before implementations can reference them (Plan 2).

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- RemoteStore interface keeps same shape as current BlockStore (WriteBlock, ReadBlock, ReadBlockRange, DeleteBlock, DeleteByPrefix, ListByPrefix, Close, HealthCheck)
- LocalStore interface composed of sub-interfaces: LocalReader, LocalWriter, LocalFlusher, LocalManager
- Both LocalStore and RemoteStore include Close() method
- fdCache stays internal to filesystem local store (pkg/blockstore/local/fs/)
- Full in-memory LocalStore implementation (pkg/blockstore/local/memory/) for tests
- FileBlock, BlockState, block helpers move from pkg/metadata/object.go to pkg/blockstore/types.go
- FileBlockStore interface moves from pkg/metadata/store.go to pkg/blockstore/
- FileBlockStore conformance tests move from pkg/metadata/storetest/ to pkg/blockstore/storetest/
- Single BlockSize constant in pkg/blockstore/ root
- FlushResult and StorageStats move to pkg/blockstore/ as blockstore.FlushResult and blockstore.Stats
- PayloadError renamed to BlockStoreError in pkg/blockstore/errors.go
- Package renamed: pkg/payload/offloader/ -> pkg/blockstore/sync/
- Import alias when Go's sync collides: `blocksync`
- Main type: Syncer (constructor: sync.New())
- File renames: upload.go -> sync.go, download.go -> fetch.go
- Method renames: uploadPendingBlocks -> syncLocalBlocks, downloadBlock -> fetchBlock, processUploadResult -> processSyncResult
- Type renames: fileUploadState -> fileSyncState, downloadResult -> fetchResult, uploadResult -> syncResult
- Queue renames: TransferQueue -> SyncQueue, TransferQueueEntry -> SyncQueueEntry
- dedup.go stays in sync/ package
- BlockStore orchestrator: struct name blockstore.BlockStore, constructor blockstore.New(cfg blockstore.Config)
- Lifecycle: explicit Start(ctx) launches goroutines, Close() stops them (two-phase init)
- Recovery runs automatically during Start() before syncer goroutines launch
- Composition: local.LocalStore (interface) + remote.RemoteStore (interface, can be nil) + *sync.Syncer (concrete) + *gc.Collector (concrete)
- Implements blockstore.Store interface (composed sub-interfaces: Reader, Writer, Flusher)
- Stats() on the interface
- Create pkg/blockstore/io/ extracting read/write from PayloadService
- Bottom-up: types/interfaces first, then implementations, then orchestrator, then consumers
- 3-4 plans, each compiles and passes tests independently
- Clean break per plan -- no temporary aliases or re-exports
- Plan 1: Types + interfaces + conformance suites in new pkg/blockstore/ hierarchy
- Plan 2: Move implementations (local/fs, remote/s3, remote/memory, sync, gc)
- Plan 3: Orchestrator (blockstore.go) + io/ + config wiring + runtime wiring
- Plan 4: Consumer updates (NFS/SMB handlers, E2E tests) + old package deletion + docs
- Conformance suites: pkg/blockstore/local/localtest/ and pkg/blockstore/remote/remotetest/
- All doc updates in final plan (Plan 4)
- doc.go per package

### Claude's Discretion
- Exact sub-interface method groupings for LocalStore (which methods go in Reader vs Writer vs Flusher vs Manager)
- Exact sub-interface method groupings for blockstore.Store (Reader vs Writer vs Flusher)
- Conformance test structure and assertions
- doc.go content and wording
- Whether to split sync.go into smaller files within pkg/blockstore/sync/

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| PKG-01 | pkg/blockstore/local/local.go defines LocalStore interface | Extracted from current BlockCache methods in pkg/cache/cache.go -- ReadAt, WriteAt, Flush, Truncate, GetFileSize, etc. Composed sub-interfaces. |
| PKG-02 | pkg/blockstore/remote/remote.go defines RemoteStore interface | Direct rename of pkg/payload/store/store.go BlockStore interface. Same 8 methods. |
| PKG-03 | pkg/cache/ moved to pkg/blockstore/local/fs/ | 14 source files (cache.go, read.go, write.go, flush.go, eviction.go, manage.go, recovery.go, block.go, types.go, fdcache.go, fadvise_linux.go, fadvise_other.go, cache_test.go, manage_test.go). Package rename from `cache` to `fs`. |
| PKG-04 | pkg/blockstore/local/memory/ created for test MemoryLocalStore | New implementation satisfying LocalStore interface. Full implementation, not a stub. |
| PKG-05 | pkg/payload/store/s3/ moved to pkg/blockstore/remote/s3/ | 2 files (store.go, store_test.go). Package name stays `s3`. Implements RemoteStore. |
| PKG-06 | pkg/payload/store/memory/ moved to pkg/blockstore/remote/memory/ | 2 files (store.go, store_test.go). Package name stays `memory`. Implements RemoteStore. |
| PKG-07 | pkg/payload/offloader/ moved to pkg/blockstore/sync/ | 12 files. Package renamed from `offloader` to `sync`. Types and methods renamed per CONTEXT.md decisions. |
| PKG-08 | pkg/payload/gc/ moved to pkg/blockstore/gc/ | 4 files (gc.go, gc_test.go, gc_integration_test.go, doc.go). Package name stays `gc`. |
| PKG-09 | pkg/blockstore/blockstore.go orchestrator absorbs PayloadService | Absorbs PayloadService.ReadAt, WriteAt, Truncate, Delete, GetSize, Exists, Flush, DrainAllUploads, GetStorageStats, HealthCheck. Composes local + remote + syncer + gc. |
| PKG-10 | All consumer imports updated | ~23 files importing pkg/payload, ~12 files importing pkg/cache. Includes NFS v3/v4 handlers, SMB handlers, runtime, config, E2E tests. |
| PKG-11 | pkg/cache/ and pkg/payload/ deleted after migration | Final deletion in Plan 4 after all consumers updated. |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go | 1.25.0 | Language runtime | Project's go.mod specifies this version |
| Standard library (`sync`, `context`, `os`, `path/filepath`) | Go 1.25 | Core concurrency, I/O, filesystem | No external dependencies needed for restructuring |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `github.com/aws/aws-sdk-go-v2` | 1.39.6 | S3 remote store | Existing dependency, moves with s3/ package |
| `github.com/dgraph-io/badger/v4` | existing | BadgerDB metadata store | FileBlockStore implementation stays in metadata |

No new dependencies are required for this phase. All work uses existing packages.

## Architecture Patterns

### Recommended Project Structure

```
pkg/blockstore/
├── blockstore.go              # BlockStore orchestrator (absorbs PayloadService)
├── types.go                   # FileBlock, BlockState, ContentHash, BlockSize
├── errors.go                  # BlockStoreError (renamed from PayloadError), sentinel errors
├── store.go                   # Store interface (Reader + Writer + Flusher + Stats)
├── doc.go                     # Package documentation
├── local/
│   ├── local.go               # LocalStore interface (LocalReader + LocalWriter + LocalFlusher + LocalManager)
│   ├── doc.go
│   ├── fs/                    # Filesystem-backed local store (moved from pkg/cache/)
│   │   ├── fs.go              # FSStore struct (was BlockCache)
│   │   ├── read.go            # ReadAt implementation
│   │   ├── write.go           # WriteAt implementation
│   │   ├── flush.go           # Flush, flushBlock
│   │   ├── eviction.go        # Disk space management
│   │   ├── manage.go          # Delete, Truncate, block management
│   │   ├── recovery.go        # Startup recovery
│   │   ├── block.go           # memBlock, blockKey, buffer pool
│   │   ├── types.go           # PendingBlock, FlushedBlock, Stats
│   │   ├── fdcache.go         # File descriptor cache
│   │   ├── fadvise_linux.go   # Linux-specific page cache hints
│   │   ├── fadvise_other.go   # No-op for non-Linux
│   │   ├── fs_test.go         # Implementation-specific tests
│   │   ├── manage_test.go     # Management tests
│   │   └── doc.go
│   ├── memory/                # In-memory local store for tests
│   │   ├── memory.go          # Full LocalStore implementation
│   │   ├── memory_test.go
│   │   └── doc.go
│   └── localtest/             # Conformance test suite for LocalStore
│       ├── suite.go
│       └── doc.go
├── remote/
│   ├── remote.go              # RemoteStore interface
│   ├── doc.go
│   ├── s3/                    # S3-backed remote store (moved from pkg/payload/store/s3/)
│   │   ├── store.go
│   │   └── store_test.go
│   ├── memory/                # In-memory remote store (moved from pkg/payload/store/memory/)
│   │   ├── store.go
│   │   └── store_test.go
│   └── remotetest/            # Conformance test suite for RemoteStore
│       ├── suite.go
│       └── doc.go
├── sync/                      # Async local-to-remote transfer (moved from pkg/payload/offloader/)
│   ├── syncer.go              # Syncer struct (was Offloader)
│   ├── sync.go                # syncLocalBlocks (was uploadPendingBlocks)
│   ├── fetch.go               # fetchBlock (was downloadBlock)
│   ├── dedup.go               # Content-addressed dedup
│   ├── queue.go               # SyncQueue (was TransferQueue)
│   ├── entry.go               # SyncQueueEntry
│   ├── types.go               # Syncer config, TransferType, etc.
│   ├── syncer_test.go
│   ├── nil_remotestore_test.go
│   ├── queue_test.go
│   ├── entry_test.go
│   └── doc.go
├── gc/                        # Block garbage collection (moved from pkg/payload/gc/)
│   ├── gc.go
│   ├── gc_test.go
│   ├── gc_integration_test.go
│   └── doc.go
├── io/                        # Cache-aware I/O extracted from PayloadService
│   ├── read.go                # ReadAt, ReadAtWithCOWSource
│   ├── write.go               # WriteAt
│   └── doc.go
└── storetest/                 # FileBlockStore conformance tests (moved from pkg/metadata/storetest/)
    ├── file_block_ops.go
    └── doc.go
```

### Pattern 1: Interface Composition (Phase 29 Pattern)

**What:** Sub-interfaces are small (2-4 methods each), exported individually so consumers accept the narrowest interface.
**When to use:** For both LocalStore and the orchestrator's Store interface.

```go
// pkg/blockstore/local/local.go

// LocalReader provides read access to the local block cache.
type LocalReader interface {
    ReadAt(ctx context.Context, payloadID string, dest []byte, offset uint64) (bool, error)
    GetFileSize(ctx context.Context, payloadID string) (uint64, bool)
    IsBlockCached(ctx context.Context, payloadID string, blockIdx uint64) bool
    GetBlockData(ctx context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error)
}

// LocalWriter provides write access to the local block cache.
type LocalWriter interface {
    WriteAt(ctx context.Context, payloadID string, data []byte, offset uint64) error
    WriteFromRemote(ctx context.Context, payloadID string, data []byte, offset uint64) error
}

// LocalFlusher handles flushing dirty data from memory to disk.
type LocalFlusher interface {
    Flush(ctx context.Context, payloadID string) ([]FlushedBlock, error)
    GetDirtyBlocks(ctx context.Context, payloadID string) ([]PendingBlock, error)
    SyncFileBlocks(ctx context.Context)
    SyncFileBlocksForFile(ctx context.Context, payloadID string)
}

// LocalManager handles lifecycle and maintenance operations.
type LocalManager interface {
    Start(ctx context.Context)
    Close() error
    Truncate(ctx context.Context, payloadID string, newSize uint64) error
    EvictMemory(ctx context.Context, payloadID string) error
    DeleteBlockFile(ctx context.Context, payloadID string, blockIdx uint64) error
    DeleteAllBlockFiles(ctx context.Context, payloadID string) error
    TruncateBlockFiles(ctx context.Context, payloadID string, newBlockCount uint64) error
    SetSkipFsync(skip bool)
    SetEvictionEnabled(enabled bool)
    Stats() Stats
    ListFiles() []string
    // Block state transitions
    MarkBlockRemote(ctx context.Context, payloadID string, blockIdx uint64) bool
    MarkBlockSyncing(ctx context.Context, payloadID string, blockIdx uint64) bool
    MarkBlockLocal(ctx context.Context, payloadID string, blockIdx uint64) bool
}

// LocalStore provides local (filesystem or memory) block caching.
// Implementations must be safe for concurrent use.
type LocalStore interface {
    LocalReader
    LocalWriter
    LocalFlusher
    LocalManager
}
```

### Pattern 2: Orchestrator Composition

**What:** The BlockStore orchestrator composes local + remote + syncer + gc, exposing a unified Store interface.
**When to use:** Plan 3 when building the orchestrator.

```go
// pkg/blockstore/blockstore.go

// Config configures a BlockStore instance.
type Config struct {
    Local    local.LocalStore
    Remote   remote.RemoteStore // nil for local-only mode
    Syncer   *sync.Syncer       // uses concrete type (owns lifecycle)
    GC       *gc.Collector      // uses concrete type (owns lifecycle)
}

// BlockStore orchestrates local cache, remote store, syncer, and GC.
type BlockStore struct {
    local  local.LocalStore
    remote remote.RemoteStore
    syncer *sync.Syncer
    gc     *gc.Collector
}

// New creates a new BlockStore. local is required, remote can be nil.
func New(cfg Config) (*BlockStore, error) { ... }

// Start launches background goroutines (syncer, GC). Recovery runs first.
func (bs *BlockStore) Start(ctx context.Context) error { ... }

// Close stops background goroutines and releases resources.
func (bs *BlockStore) Close() error { ... }
```

### Pattern 3: Clean Break Moves

**What:** Each plan updates ALL consumers in one atomic commit. No temporary re-exports or aliases.
**When to use:** Every plan in this phase.

The key insight is that Go's compile-time type checking makes partial moves impossible without aliases. The bottom-up ordering avoids this: Plan 1 creates new types/interfaces that don't break existing code (nothing references them yet). Plan 2 moves implementations to use the new types. Plan 3 creates the orchestrator using the moved implementations. Plan 4 updates all consumers to use the new paths and deletes old packages.

### Anti-Patterns to Avoid
- **Circular imports:** `pkg/blockstore/types.go` will define FileBlock and BlockState. The metadata store implementations (memory, badger, postgres) must import blockstore for FileBlockStore interface. Ensure blockstore does NOT import metadata. The dependency arrow is: metadata -> blockstore (for types), NOT the reverse.
- **Partial moves:** Moving half the files in one commit but leaving consumers pointing to old paths causes compilation failures. Each plan must be fully self-consistent.
- **Re-export shims:** Previous phases used clean breaks successfully. Don't add `var BlockSize = blockstore.BlockSize` style re-exports.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Package moves | Manual find-and-replace | `gorename` or IDE refactoring + manual review | Go's import system catches all breakage at compile time |
| Interface conformance | Runtime type checks | Compile-time `var _ Interface = (*Impl)(nil)` | Catches interface drift immediately |
| Circular dependency detection | Manual graph walking | `go build ./...` | Go compiler refuses to build circular imports |

**Key insight:** Go's compile-time import checking is the best tool for this phase. Every broken import is a compile error. `go build ./...` is the verification gate, and `go test ./...` validates behavior preservation.

## Common Pitfalls

### Pitfall 1: Circular Import Between blockstore and metadata
**What goes wrong:** FileBlock currently lives in `pkg/metadata/`. Moving it to `pkg/blockstore/` means metadata store implementations (memory, badger, postgres) must import `pkg/blockstore` for the type. If `pkg/blockstore` also imports `pkg/metadata` (e.g., for MetadataStore, PayloadID, AuthContext), you get a circular import.
**Why it happens:** FileBlock and FileBlockStore are currently part of the metadata package, and the metadata MetadataStore interface embeds FileBlockStore.
**How to avoid:** When moving FileBlock/FileBlockStore to blockstore, the MetadataStore interface in `pkg/metadata/store.go` must import `pkg/blockstore` and embed `blockstore.FileBlockStore`. The blockstore package must NOT import metadata. PayloadID (currently `metadata.PayloadID`) is used in PayloadService -- the blockstore orchestrator should accept `string` directly (which PayloadID already is -- it's a `type PayloadID = string` alias).
**Warning signs:** `import cycle not allowed` compiler error.

### Pitfall 2: Test File Build Tags
**What goes wrong:** Integration test files (`gc_integration_test.go`, `blockstore_integration_test.go`) have `//go:build integration` tags. Moving them must preserve these tags or they'll run in normal `go test ./...` and fail.
**Why it happens:** Copy-paste omitting the build tag line.
**How to avoid:** Verify build tags are preserved in moved files. The E2E tests use `//go:build e2e` tags.
**Warning signs:** Test failures referencing Docker/Localstack when running `go test ./...`.

### Pitfall 3: Package Name vs Directory Name Mismatch
**What goes wrong:** Go requires the package name to match the directory name (by convention). `pkg/blockstore/sync/` must use `package sync` which collides with the standard library `sync` package.
**Why it happens:** The `sync` package name is already taken by the stdlib.
**How to avoid:** The CONTEXT.md decision addresses this: import alias `blocksync` when importing. The package declaration is `package sync` (matching directory name), and consumers use `import blocksync "github.com/marmos91/dittofs/pkg/blockstore/sync"`. Within the sync package itself, internal references to Go's `sync` stdlib need a different import alias (e.g., `gosync "sync"`).
**Warning signs:** `sync.WaitGroup` resolving to the wrong package inside the sync package.

### Pitfall 4: metadata.MetadataStore Still Embedding FileBlockStore
**What goes wrong:** After moving FileBlockStore to blockstore, the MetadataStore interface in `pkg/metadata/store.go` must change from embedding `FileBlockStore` (local) to `blockstore.FileBlockStore` (imported). This changes the type signature for ALL metadata store implementations.
**Why it happens:** The embedded interface type changes package.
**How to avoid:** In Plan 1, move the FileBlockStore interface to `pkg/blockstore/` and update `pkg/metadata/store.go` to import and embed `blockstore.FileBlockStore`. Then update all three metadata store implementations (memory, badger, postgres) to import blockstore for the interface. Since the methods remain identical, the implementations satisfy the interface without code changes.
**Warning signs:** `does not implement blockstore.FileBlockStore` compiler error.

### Pitfall 5: FormatStoreKey Reference Chain
**What goes wrong:** `cache.FormatStoreKey()` is used by the offloader's upload.go. When cache moves to local/fs, the function must either move to a shared location or the sync package must import local/fs.
**Why it happens:** FormatStoreKey is a utility function that both the local store and the syncer need.
**How to avoid:** Move FormatStoreKey to `pkg/blockstore/` root (it's a simple format function with no dependencies). Both local/fs and sync/ import blockstore root.
**Warning signs:** Import of local/fs from sync/ creating unexpected coupling.

### Pitfall 6: E2E Test Store Helpers Still Use Old CLI
**What goes wrong:** `test/e2e/helpers/stores.go` has PayloadStore type and CreatePayloadStore/ListPayloadStores methods that use `store payload add` CLI commands. Phase 44 added `store block` CLI commands, but E2E helpers weren't updated.
**Why it happens:** E2E test helpers weren't in scope for Phase 44.
**How to avoid:** In Plan 4, update E2E test helpers to use the new `store block local/remote` CLI commands. The old `store payload` commands may still work as backward compatibility, but tests should use the canonical paths.
**Warning signs:** E2E tests passing with wrong CLI commands, masking the actual migration.

## Code Examples

### LocalStore Sub-Interface Groupings (Recommended)

Based on analysis of BlockCache's 30+ public methods, here is the recommended grouping:

```go
// LocalReader: Read-path methods (used by NFS READ handlers)
// - ReadAt(ctx, payloadID, dest, offset) (bool, error)
// - GetFileSize(ctx, payloadID) (uint64, bool)
// - IsBlockCached(ctx, payloadID, blockIdx) bool
// - GetBlockData(ctx, payloadID, blockIdx) ([]byte, uint32, error)

// LocalWriter: Write-path methods (used by NFS WRITE handlers)
// - WriteAt(ctx, payloadID, data, offset) error
// - WriteFromRemote(ctx, payloadID, data, offset) error

// LocalFlusher: Flush/sync methods (used by NFS COMMIT + syncer)
// - Flush(ctx, payloadID) ([]FlushedBlock, error)
// - GetDirtyBlocks(ctx, payloadID) ([]PendingBlock, error)
// - SyncFileBlocks(ctx)
// - SyncFileBlocksForFile(ctx, payloadID)

// LocalManager: Lifecycle + management (used by orchestrator)
// - Start(ctx)
// - Close() error
// - Truncate(ctx, payloadID, newSize) error
// - EvictMemory(ctx, payloadID) error
// - Delete operations (DeleteBlockFile, DeleteAllBlockFiles, TruncateBlockFiles)
// - Configuration (SetSkipFsync, SetEvictionEnabled)
// - Observability (Stats(), ListFiles())
// - Block state transitions (MarkBlockRemote, MarkBlockSyncing, MarkBlockLocal)
```

### BlockStore Store Interface (Recommended)

```go
// pkg/blockstore/store.go

// Reader provides read access to block storage.
type Reader interface {
    ReadAt(ctx context.Context, payloadID string, data []byte, offset uint64) (int, error)
    ReadAtWithCOWSource(ctx context.Context, payloadID string, cowSource string, data []byte, offset uint64) (int, error)
    GetSize(ctx context.Context, payloadID string) (uint64, error)
    Exists(ctx context.Context, payloadID string) (bool, error)
}

// Writer provides write access to block storage.
type Writer interface {
    WriteAt(ctx context.Context, payloadID string, data []byte, offset uint64) error
    Truncate(ctx context.Context, payloadID string, newSize uint64) error
    Delete(ctx context.Context, payloadID string) error
}

// Flusher handles data persistence operations.
type Flusher interface {
    Flush(ctx context.Context, payloadID string) (*FlushResult, error)
    DrainAllUploads(ctx context.Context) error
}

// Store is the complete block storage interface.
type Store interface {
    Reader
    Writer
    Flusher
    Stats() (*Stats, error)
    HealthCheck(ctx context.Context) error
    Start(ctx context.Context) error
    Close() error
}
```

### Syncer Rename Map

| Old (pkg/payload/offloader/) | New (pkg/blockstore/sync/) |
|------------------------------|---------------------------|
| `Offloader` struct | `Syncer` struct |
| `offloader.New()` | `sync.New()` |
| `offloader.go` | `syncer.go` |
| `upload.go` | `sync.go` |
| `download.go` | `fetch.go` |
| `uploadPendingBlocks()` | `syncLocalBlocks()` |
| `downloadBlock()` | `fetchBlock()` |
| `processUploadResult()` | `processSyncResult()` |
| `fileUploadState` | `fileSyncState` |
| `downloadResult` | `fetchResult` |
| `uploadResult` | `syncResult` |
| `TransferQueue` | `SyncQueue` |
| `TransferQueueEntry` | `SyncQueueEntry` |
| `offloader_test.go` | `syncer_test.go` |
| `nil_blockstore_test.go` | `nil_remotestore_test.go` |
| `ErrClosed` | `ErrClosed` (unchanged) |
| `Config` | `Config` (unchanged) |

### Import Alias Pattern for sync Package

```go
// In files that import both Go's sync and blockstore sync:
import (
    "sync"  // Go standard library -- used for sync.WaitGroup, sync.Mutex

    blocksync "github.com/marmos91/dittofs/pkg/blockstore/sync"
)

// Inside pkg/blockstore/sync/ package itself:
package sync

import (
    gosync "sync"  // Alias Go's sync to avoid shadowing
)

// Use gosync.WaitGroup, gosync.Mutex, etc. inside the package
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| `pkg/cache/` + `pkg/payload/` separate hierarchies | `pkg/blockstore/` unified hierarchy | Phase 45 (now) | Single import root for all block storage |
| `PayloadService` orchestrator | `blockstore.BlockStore` orchestrator | Phase 45 (now) | Cleaner naming, composed sub-interfaces |
| `Offloader` terminology | `Syncer` with sync/fetch terminology | Phase 41 (logs) + Phase 45 (code) | Consistent domain language |
| `BlockStore` (for remote) | `RemoteStore` (specific) | Phase 45 (now) | Avoids confusion with BlockStore orchestrator |
| FileBlock types in metadata pkg | FileBlock types in blockstore pkg | Phase 45 (now) | Types live in their domain package |
| PayloadError | BlockStoreError | Phase 45 (now) | Consistent naming |

## Dependency Graph Analysis

Understanding the dependency flow is critical for avoiding circular imports:

### Current Dependencies
```
pkg/payload/service.go → pkg/cache, pkg/payload/offloader
pkg/payload/offloader/ → pkg/cache, pkg/payload/store, pkg/metadata
pkg/payload/gc/ → pkg/metadata, pkg/payload/store
pkg/cache/ → pkg/metadata (for FileBlock, FileBlockStore, BlockState)
internal/adapter/nfs/v3/handlers/ → pkg/payload, pkg/metadata
internal/adapter/nfs/v4/handlers/ → pkg/payload, pkg/metadata
pkg/controlplane/runtime/ → pkg/cache, pkg/payload, pkg/payload/offloader, pkg/payload/store
```

### Target Dependencies (After Phase 45)
```
pkg/blockstore/ (root) → NO external deps (only defines types + interfaces)
pkg/blockstore/local/ → pkg/blockstore (for types)
pkg/blockstore/local/fs/ → pkg/blockstore, pkg/blockstore/local (for LocalStore types)
pkg/blockstore/local/memory/ → pkg/blockstore, pkg/blockstore/local
pkg/blockstore/remote/ → pkg/blockstore (for types)
pkg/blockstore/remote/s3/ → pkg/blockstore/remote
pkg/blockstore/remote/memory/ → pkg/blockstore/remote
pkg/blockstore/sync/ → pkg/blockstore, pkg/blockstore/local, pkg/blockstore/remote
pkg/blockstore/gc/ → pkg/blockstore, pkg/blockstore/remote
pkg/blockstore/io/ → pkg/blockstore/local, pkg/blockstore/sync
pkg/blockstore/ (orchestrator) → all sub-packages

pkg/metadata/store.go → pkg/blockstore (for FileBlockStore interface)
pkg/metadata/store/memory/ → pkg/blockstore (for FileBlock types)
pkg/metadata/store/badger/ → pkg/blockstore (for FileBlock types)
pkg/metadata/store/postgres/ → pkg/blockstore (for FileBlock types)

internal/adapter/nfs/*/handlers/ → pkg/blockstore
pkg/controlplane/runtime/ → pkg/blockstore
```

### Critical: No Circular Imports
- `pkg/blockstore` MUST NOT import `pkg/metadata` (breaks the cycle)
- `pkg/metadata` imports `pkg/blockstore` (for FileBlockStore, FileBlock, BlockState)
- This reverses the current direction where cache/payload import metadata

## File Inventory

### Files to Move (with target locations)

**pkg/cache/ -> pkg/blockstore/local/fs/** (14 files)
| Source | Target | Package Rename |
|--------|--------|---------------|
| cache.go | fs.go | `cache` -> `fs` |
| read.go | read.go | `cache` -> `fs` |
| write.go | write.go | `cache` -> `fs` |
| flush.go | flush.go | `cache` -> `fs` |
| eviction.go | eviction.go | `cache` -> `fs` |
| manage.go | manage.go | `cache` -> `fs` |
| recovery.go | recovery.go | `cache` -> `fs` |
| block.go | block.go | `cache` -> `fs` |
| types.go | types.go | `cache` -> `fs` |
| fdcache.go | fdcache.go | `cache` -> `fs` |
| fadvise_linux.go | fadvise_linux.go | `cache` -> `fs` |
| fadvise_other.go | fadvise_other.go | `cache` -> `fs` |
| cache_test.go | fs_test.go | `cache` -> `fs` |
| manage_test.go | manage_test.go | `cache` -> `fs` |

**pkg/payload/store/s3/ -> pkg/blockstore/remote/s3/** (2 files)
**pkg/payload/store/memory/ -> pkg/blockstore/remote/memory/** (2 files)

**pkg/payload/offloader/ -> pkg/blockstore/sync/** (12 files, with renames)
| Source | Target |
|--------|--------|
| offloader.go | syncer.go |
| upload.go | sync.go |
| download.go | fetch.go |
| dedup.go | dedup.go |
| queue.go | queue.go |
| entry.go | entry.go |
| types.go | types.go |
| doc.go | doc.go |
| offloader_test.go | syncer_test.go |
| nil_blockstore_test.go | nil_remotestore_test.go |
| queue_test.go | queue_test.go |
| entry_test.go | entry_test.go |

**pkg/payload/gc/ -> pkg/blockstore/gc/** (4 files)

**pkg/metadata/object.go (partial) -> pkg/blockstore/types.go** (extract FileBlock, BlockState, ContentHash)
**pkg/metadata/store.go (partial) -> pkg/blockstore/ root** (extract FileBlockStore interface)
**pkg/metadata/storetest/file_block_ops.go -> pkg/blockstore/storetest/** (extract)

### Files to Create (new)
- `pkg/blockstore/types.go` - FileBlock, BlockState, ContentHash, BlockSize
- `pkg/blockstore/errors.go` - BlockStoreError, sentinel errors
- `pkg/blockstore/store.go` - Store interface (Reader + Writer + Flusher)
- `pkg/blockstore/blockstore.go` - Orchestrator
- `pkg/blockstore/local/local.go` - LocalStore interface
- `pkg/blockstore/remote/remote.go` - RemoteStore interface
- `pkg/blockstore/local/memory/memory.go` - MemoryLocalStore
- `pkg/blockstore/local/localtest/suite.go` - LocalStore conformance
- `pkg/blockstore/remote/remotetest/suite.go` - RemoteStore conformance
- `pkg/blockstore/io/read.go` - Read I/O logic
- `pkg/blockstore/io/write.go` - Write I/O logic
- `pkg/blockstore/storetest/file_block_ops.go` - FileBlockStore conformance
- Multiple `doc.go` files

### Consumer Files to Update (~23+ files)
- `internal/adapter/nfs/v3/handlers/utils.go`
- `internal/adapter/nfs/v3/handlers/read.go`
- `internal/adapter/nfs/v3/handlers/read_payload.go`
- `internal/adapter/nfs/v3/handlers/write.go`
- `internal/adapter/nfs/v3/handlers/create.go`
- `internal/adapter/nfs/v3/handlers/commit.go`
- `internal/adapter/nfs/v3/handlers/remove.go`
- `internal/adapter/nfs/v3/handlers/testing/fixtures.go`
- `internal/adapter/nfs/v4/handlers/helpers.go`
- `internal/adapter/nfs/v4/handlers/read.go`
- `internal/adapter/nfs/v4/handlers/write.go`
- `internal/adapter/nfs/v4/handlers/close.go`
- `internal/adapter/nfs/v4/handlers/commit.go`
- `internal/adapter/nfs/v4/handlers/io_test.go`
- `internal/adapter/smb/v2/handlers/flush.go`
- `pkg/controlplane/runtime/runtime.go`
- `pkg/controlplane/runtime/init.go`
- `pkg/controlplane/runtime/init_test.go`
- `pkg/controlplane/runtime/runtime_test.go`
- `pkg/controlplane/runtime/shares/service.go`
- `cmd/dfs/commands/start.go`
- `pkg/payload/store/blockstore_integration_test.go` (moves or deletes)
- `test/e2e/helpers/stores.go` + ~35 E2E test files

### Files/Directories to Delete (Plan 4)
- `pkg/cache/` (entire directory)
- `pkg/payload/` (entire directory)
- `pkg/payload/store/store.go` (BlockStore interface moves to remote.go)
- `pkg/payload/README.md`

## Open Questions

1. **metadata.PayloadID type**
   - What we know: `PayloadID` is `type PayloadID = string` (a type alias). The blockstore orchestrator uses it as a plain string.
   - What's unclear: Whether to keep using `metadata.PayloadID` in blockstore signatures or switch to plain `string` to avoid importing metadata.
   - Recommendation: Use plain `string` in blockstore interfaces. PayloadID is a type alias (not a distinct type), so `string` is fully compatible. This avoids the circular import risk.

2. **FileBlock types remaining in metadata after move**
   - What we know: ContentHash and ObjectID (which is `= ContentHash`) are used in metadata for other purposes (Object store).
   - What's unclear: Whether ContentHash should stay in metadata and be duplicated in blockstore, or moved entirely.
   - Recommendation: Move ContentHash to blockstore (it's primarily a block concept). ObjectID in metadata becomes `type ObjectID = blockstore.ContentHash`. This keeps the single source of truth in blockstore.

3. **MetadataStore.Transaction embedding FileBlockStore**
   - What we know: The Transaction interface in metadata/store.go embeds FileBlockStore. After the move, it must embed `blockstore.FileBlockStore`.
   - What's unclear: Whether this creates any subtle issues with transaction isolation.
   - Recommendation: The embedding change is purely a type-level change. Transaction implementations already satisfy the methods. No behavioral change needed.

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go testing (stdlib) |
| Config file | None (go test uses convention) |
| Quick run command | `go test ./pkg/blockstore/...` |
| Full suite command | `go build ./... && go test ./...` |

### Phase Requirements -> Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| PKG-01 | LocalStore interface defined | unit | `go build ./pkg/blockstore/local/` | No - Wave 0 |
| PKG-02 | RemoteStore interface defined | unit | `go build ./pkg/blockstore/remote/` | No - Wave 0 |
| PKG-03 | Cache code moved to local/fs | unit | `go test ./pkg/blockstore/local/fs/` | No - Wave 0 |
| PKG-04 | Memory LocalStore created | unit | `go test ./pkg/blockstore/local/memory/` | No - Wave 0 |
| PKG-05 | S3 store moved to remote/s3 | unit | `go build ./pkg/blockstore/remote/s3/` | No - Wave 0 |
| PKG-06 | Memory store moved to remote/memory | unit | `go test ./pkg/blockstore/remote/memory/` | No - Wave 0 |
| PKG-07 | Offloader moved to sync/ | unit | `go test ./pkg/blockstore/sync/` | No - Wave 0 |
| PKG-08 | GC moved | unit | `go test ./pkg/blockstore/gc/` | No - Wave 0 |
| PKG-09 | Orchestrator absorbs PayloadService | unit | `go test ./pkg/blockstore/` | No - Wave 0 |
| PKG-10 | All imports updated | build | `go build ./...` | N/A |
| PKG-11 | Old packages deleted | build | `go build ./... && ! test -d pkg/cache && ! test -d pkg/payload` | N/A |

### Sampling Rate
- **Per task commit:** `go build ./... && go test ./...`
- **Per wave merge:** `go build ./... && go test ./... && go vet ./...`
- **Phase gate:** Full suite green before `/gsd:verify-work`

### Wave 0 Gaps
None -- test infrastructure (Go testing) is already in place. New test files are created as part of each plan (conformance suites, moved tests). No additional framework setup needed.

## Sources

### Primary (HIGH confidence)
- Direct codebase analysis of all source files in pkg/cache/, pkg/payload/, pkg/metadata/
- go.mod confirming Go 1.25.0 and dependency versions
- Phase 45 CONTEXT.md with locked decisions
- REQUIREMENTS.md with PKG-01 through PKG-11 specifications

### Secondary (MEDIUM confidence)
- Phase 29, 41, 42, 43, 44 patterns observed in STATE.md and codebase (clean break moves, interface composition, conformance suites)

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - Pure Go refactoring, no new dependencies
- Architecture: HIGH - All source and target locations verified in codebase, dependency graph analyzed for circular import safety
- Pitfalls: HIGH - Every pitfall identified from direct code analysis, especially the circular import risk and sync package naming collision

**Research date:** 2026-03-09
**Valid until:** 2026-04-09 (stable - internal refactoring, no external dependency changes)
