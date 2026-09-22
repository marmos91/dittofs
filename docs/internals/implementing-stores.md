# Implementing Custom Stores

This guide provides comprehensive instructions for implementing custom metadata stores and block stores for DittoFS. Whether you're building a database-backed metadata store or a custom cloud storage integration, this document will walk you through the process with best practices and practical examples.

## Table of Contents

1. [Overview](#overview)
2. [When to Implement Custom Stores](#when-to-implement-custom-stores)
3. [Understanding the Architecture](#understanding-the-architecture)
4. [Implementing Metadata Stores](#implementing-metadata-stores)
5. [The journal (local tier)](#the-journal-local-tier)
6. [Implementing a Remote Store](#implementing-a-remote-store)
7. [Implementing a Remote Block Store (block-keyed)](#implementing-a-remote-block-store-block-keyed)
8. [Best Practices](#best-practices)
9. [Testing Your Implementation](#testing-your-implementation)
10. [Common Pitfalls](#common-pitfalls)
11. [Integration with DittoFS](#integration-with-dittofs)

## Overview

There are **two** store types an operator configures and therefore two you can
implement:

- **Metadata Stores**: Simple CRUD operations for file/directory structure, attributes, permissions
- **Block Stores**: Durable storage in S3 or a compatible object store, shared across shares via ref counting

The local tier is **not** one of them. Every share keeps an on-disk **journal**
(`pkg/block/journal/`) in front of its block store; it is provisioned
automatically under `blockstore.journal.path` and has no type, no kind, and no
operator-facing configuration. A block store has no kind either — the old
local/remote split is gone, and the valid types are `s3` and `memory`. If you are
here looking for how to implement a custom *local* store type, that concept no
longer exists; see [The journal](#the-journal-local-tier) for what replaced it.

**Key Design Principle**: Each share gets its own `*engine.BlockStore` instance
that composes its journal, its one block store, and a syncer. The engine
orchestrates reads and writes across the two tiers.

This separation enables:
- Independent scaling of metadata and block storage
- Per-share isolation with shared block-store backends
- Simple store implementations (just implement the interface, the engine handles coordination)

## When to Implement Custom Stores

### Metadata Store Use Cases

Implement a custom metadata store when you need:

- **Database-backed storage**: PostgreSQL, MySQL, MongoDB, Cassandra
- **Distributed metadata**: Multi-node coordination, consensus protocols
- **Advanced features**: Full-text search, custom indexing, complex queries
- **Compliance**: Audit logs, versioning, immutability guarantees

**Example**: A PostgreSQL-backed metadata store for enterprise environments requiring audit trails and high availability.

### Block Store Use Cases

Implement a custom block store when you need:

- **Cloud storage integration**: Azure Blob, Google Cloud Storage, custom object stores
- **Specialized storage**: Tape archives, HSM systems, data lakes
- **Tiering**: Automatic hot/cold data movement based on access patterns

**Reference implementation**: `pkg/block/remote/s3/` (S3-backed block store)

There is no "local store use case" row any more. The journal is the only local
tier and it is not pluggable by configuration; `pkg/block/local` remains as an
internal interface with exactly two implementations (`*journal.Store` in
production, `local/memory` for tests).

## Understanding the Architecture

### Per-Share BlockStore

Each share gets its own `*engine.BlockStore` instance:

```
┌─────────────────────────────────────┐
│  engine.BlockStore (per-share)      │
│                                     │
│  ┌─────────────┐  ┌─────────────┐  │
│  │   Journal   │  │ Block Store │  │
│  │  (always)   │  │  (always)   │  │
│  └──────┬──────┘  └──────┬──────┘  │
│         │                │          │
│         └───────┬────────┘          │
│                 │                   │
│          ┌──────▼──────┐            │
│          │   Syncer    │            │
│          │ (async xfer)│            │
│          └─────────────┘            │
└─────────────────────────────────────┘
```

- **Journal** — all reads and writes go through it first. One per share, under
  `blockstore.journal.path/shares/<share-name>/journal/`.
- **Block store** — one per share, the durable copy. The Syncer asynchronously
  offloads journal blocks to it.
- **Ref counting**: Block stores are shared across shares; when the last share
  using one is removed, the connection is closed.

### File Handle and Block Resolution

Protocol handlers resolve the per-share block store via `GetBlockStoreForHandle(ctx, handle)`:

1. File handle encodes the share name
2. Runtime extracts share name and returns the share's BlockStore
3. Handler calls `ReadAt` / `WriteAt` on the BlockStore

## Implementing Metadata Stores

The metadata store interface and implementation guide remains the same as before. See the `pkg/metadata/Store` interface and reference implementations:

- `pkg/metadata/store/memory/`: In-memory (fast, ephemeral)
- `pkg/metadata/store/badger/`: BadgerDB (persistent, embedded)
- `pkg/metadata/store/sqlite/`: SQLite (persistent, embedded)
- `pkg/metadata/store/postgres/`: PostgreSQL (persistent, distributed)

The two SQL backends are one implementation over two dialects. Most
operation bodies live once in `pkg/metadata/store/sql/`, embedded by both the
store and its transaction so a method written there is reachable from either.
The `sqlite/` and `postgres/` packages carry what genuinely differs —
connection setup, driver error mapping, statement text (via the
`sql.Dialect` interface), snapshot export, and the handful of bodies whose
mechanism diverges, such as `ApplyDataWrite` (sqlite selects then updates
under the single-writer lock; postgres folds both into one statement) —
along with the store-level wrappers described next. Read
`pkg/metadata/store/sql/` first when tracing a SQL backend; most of what a
caller reaches is there, not in the dialect package.

**Multi-statement work needs a transaction, and `Core` cannot supply one.**
`Core` is embedded by the pool-backed store as well as by the transaction, so
a `Core` method also runs on the pool, where each statement autocommits
independently and a crash between two of them leaves torn state. There are
two ways to keep that from happening, and both are in the tree:

- Make it a package-level function taking `(ctx, x Executor, d Dialect, ...)`,
  so it can only be called with an executor the caller has already chosen —
  `PutFileChunkRefs`, `DecrementAndReapMany`, `PutSyncedLocators`.
- Or write it as a `Core` method and **shadow it** in each dialect package with
  a store-level wrapper that runs it inside `WithTransaction` —
  `DeleteShare`, `CreateRootDirectory`, `DecrementRefCountAndReap`.

The shadows are load-bearing and easy to delete by accident: removing one does
not break the build, because the promoted `Core` method still satisfies the
interface. The write simply starts going straight to the pool. If you add a
multi-statement `Core` method, add its shadow to **both** dialect packages in
the same change, or use the package-level form instead. Single-statement work
is safe as a plain `Core` method.

Conformance tests: `pkg/metadata/storetest/`

### MetadataStore.EnumerateFileChunks

`MetadataStore` carries a mandatory cursor method that the mark-sweep
garbage collector uses to enumerate every live block hash without loading
the full file/block set into application memory.

> **Note:** `EnumerateFileChunks` lives on `MetadataStore`, not
> `FileChunkStore`. Conceptually it iterates across files for the GC mark
> phase — a metadata-store-wide concern — so it sits above the narrow
> `FileChunkStore` surface (see [FileChunkStore narrowing](#filechunkstore-narrowing)
> below).

```go
// EnumerateFileChunks streams every FileChunk's ContentHash to fn.
// Implementations MUST:
//   - Iterate using a backend-native cursor (Badger prefix iterator,
//     Postgres server-side cursor with batched fetch, in-memory map
//     iteration) -- no full-set load.
//   - Honor ctx.Done(): return ctx.Err() promptly when the context is
//     cancelled.
//   - Emit zero-hash FileChunks the same way as non-zero-hash blocks;
//     the GC live-set ignores zero hashes.
//   - Abort iteration and return the fn error verbatim if fn returns
//     non-nil; do NOT swallow it.
//   - Be safe under concurrent writes: it is acceptable for the cursor
//     to miss FileChunks created mid-iteration; the next mark cycle
//     will pick them up.
EnumerateFileChunks(ctx context.Context, fn func(ContentHash) error) error
```

Conformance scenarios live in `pkg/metadata/storetest/` and every
backend MUST pass them:

1. **Empty store**: `fn` is never invoked; returns `nil`.
2. **Single file**: `fn` is invoked once per FileChunk for the file.
3. **Large fanout** (`N` files × `M` blocks): `fn` is invoked exactly
   `N*M` times in any order; no duplicates, no omissions.
4. **fn-error mid-iteration**: returning a non-nil error from `fn`
   aborts iteration and propagates the error.
5. **Context cancellation**: cancelling `ctx` mid-iteration causes
   the call to return `ctx.Err()` within the polling interval.

Memory-store reference: direct `range` over the in-memory map.
Badger-store reference: `txn.NewIterator` over the FileChunk prefix.
Postgres-store reference: server-side cursor (`DECLARE` + `FETCH`)
with batches of 1000 rows.

### FileChunkStore narrowing

`pkg/block.FileChunkStore` is the narrow public FileChunk surface.
Backend implementations are simpler than the engine-internal helpers, which
live on a separate wider interface.

```go
// pkg/block/fileblock.go
type FileChunkStore interface {
    // GetByHash returns any FileChunk with the given content hash, or
    // (nil, nil) when absent (multiple rows may share a hash; best-effort).
    GetByHash(ctx context.Context, hash ContentHash) (*FileChunk, error)

    // Put creates or replaces a FileChunk by ID (upsert by ID, not hash).
    Put(ctx context.Context, block *FileChunk) error

    // Delete removes a FileChunk by ID. Returns ErrFileChunkNotFound if absent.
    Delete(ctx context.Context, id string) error

    // IncrementRefCount atomically bumps RefCount for the given FileChunk id.
    IncrementRefCount(ctx context.Context, id string) error

    // DecrementRefCount atomically decrements; returns the new count.
    DecrementRefCount(ctx context.Context, id string) (uint32, error)

    // DecrementRefCountAndReap atomically decrements and, if the count hits 0,
    // deletes the row in the same critical section. Returns the new count.
    DecrementRefCountAndReap(ctx context.Context, id string) (uint32, error)

    // AddRef atomically increments RefCount on the row indexed by hash
    // (the dedup LRU hit path). Returns ErrUnknownHash if no row exists.
    AddRef(ctx context.Context, hash ContentHash, payloadID string, blockRef BlockRef) error
}
```

**Engine-internal companion interface:** `pkg/block.EngineFileChunkStore`
extends `FileChunkStore` with `GetFileChunk(ctx, id)` and
`ListFileChunks(ctx, payloadID)` for the engine's hot paths. All four built-in
backends (memory, badger, sqlite, postgres) satisfy it without changes — the
narrow public surface is a documentation concern, not a runtime
restriction. Custom backends implementing `FileChunkStore` SHOULD also
implement the engine-internal helpers if they intend to slot into the
`*engine.BlockStore`.

**Internal storage shape is up to the backend.** The built-in schema
(`id VARCHAR PRIMARY KEY` + `hash` non-unique index) permits multiple rows
per hash for older data; the public `GetByHash` surface hides that detail.

### FileAttr.Blocks []BlockRef

`FileAttr.Blocks []blockstore.BlockRef` is the authoritative content list
for every file. `BlockRef` is the 3-tuple `(Hash, Offset, Size)` — see
`pkg/block/types.go`. The list MUST be sorted by `Offset` and is
populated on every sync finalization.

Encoding requirements per backend:

- **Postgres**: a separate `file_block_refs` join table keyed by
  `(file_id, offset)`, with `INCLUDE (size, hash)` for index-only scans
  on the read hot path. Foreign key `file_id REFERENCES files(id) ON
  DELETE CASCADE` provides a safety net — the engine still decrements
  `file_blocks.RefCount` for every BlockRef BEFORE deleting the file;
  cascade catches engine-bug paths that miss the explicit decrement.
  Hash column is `BYTEA` (32 bytes), not hex `TEXT`.
- **Badger** and **Memory**: inline-encode `Blocks []BlockRef` inside
  the existing `FileAttr` blob. Badger goes through
  `pkg/metadata/store/badger/encoding.go` (gob); Memory holds typed
  structs directly. Use `omitempty` so older blobs decode cleanly with an
  empty `Blocks` slice.

A new metadata-store method persists the list; in the built-in
backends this is `MetadataStore.SetFileChunks(ctx, handle, []BlockRef,
authCtx) error`. Custom metadata backends MUST persist atomically
with the same transaction that updates `Size`/`Mtime`/`Ctime` — the
engine relies on caller-side metadata-txn isolation rather than a
per-chunk metadata roundtrip.

#### Conformance scenarios

The `pkg/metadata/storetest/` suite includes:

1. **BlockRef round-trip**: `SetFileChunks` followed by `GetFileAttr`
   returns the same offset-sorted slice, byte-for-byte.
2. **Empty / legacy compat**: `FileAttr` blobs without a `Blocks`
   field decode to an empty slice without errors.
3. **FK cascade (Postgres-only)**: deleting a file removes all
   matching `file_block_refs` rows.
4. **Refcount reconcile**: `∑ FileChunk.RefCount` over the FileChunkStore
   equals `∑ len(FileAttr.Blocks)` over the MetadataStore at every
   quiescent point.
5. **Refcount concurrent fuzz** (`pkg/metadata/storetest/inv02_fuzz_test.go`):
   100-iteration property-based fuzzer creating, deleting, and copying
   files concurrently; asserts the invariant after each operation
   batch. Runs against all four built-in backends and any custom backend
   wired into the conformance harness.

### FileAttr.ObjectID + FindByObjectID

`FileAttr.ObjectID` is a BLAKE3 Merkle root over `BlockRef.Hash` values
sorted by `Offset`, prefixed by the domain-separation tag
`dittofs:objectid:v1\x00`. Computed by `blockstore.ComputeObjectID` and
persisted at every full quiesce in the same metadata transaction that
updates `Blocks`/`Size`/`Mtime`.

Lifecycle: cleared (zeroed) on first dirty write that mutates `Blocks`,
recomputed at next full quiesce (every block transitioned to `Remote`).
Partial flushes leave `ObjectID` at zero so the lookup index never
returns a half-quiesced file.

#### `FindByObjectID(ctx, ObjectID) ([]BlockRef, error)`

The file-level dedup short-circuit primitive. Looks up a file by its
Merkle-root ObjectID. Returns `(nil, nil)` on miss; a non-nil result
carries the canonical BlockRef list of the matching file (per-metadata-
store scope, NOT per-share).

Backends MUST maintain a secondary index from ObjectID to file row:

| Backend  | Index                                                                       |
|----------|-----------------------------------------------------------------------------|
| Postgres | Partial unique: `files_object_id_idx ON files(object_id) WHERE object_id IS NOT NULL` |
| Badger   | Secondary key `obj:{hex} -> file_id`, maintained inside each `Put`/`Delete` write batch |
| Memory   | `map[ContentHash]uuid`, guarded by the existing store mutex                 |

Zero-valued ObjectID (legacy / pre-quiesce) MUST NOT match any row —
implementations short-circuit and return `(nil, nil)` on zero input.

The unique constraint enforces first-committer-wins on a concurrent
quiesce. On race, the loser surfaces a backend-specific
unique-violation error that the runtime coordinator wraps into the
shared `metadata.ErrConflict` sentinel.

A test-only optional capability `ObjectIDIndexAccessor.CountObjectIDIndexRows`
is exercised by the storetest `ConcurrentQuiesceRace` scenario;
backends implement it inline (e.g., `SELECT count(*)` for Postgres,
`txn.Get(keyObjectID(oid))`-shape for Badger, direct map probe for
Memory). Production code MUST NOT call it.

Conformance scenarios live in
`pkg/metadata/storetest/objectid_roundtrip.go` and
`pkg/metadata/storetest/objectid_lookup.go`. All built-in backends pass
without per-backend `t.Skip` (the `ObjectIDIndexAccessor` capability is
the only legitimate type-assertion-skip; backends without that accessor
are still required to pass the functional scenarios).

### Block Record Store

`BlockRecordStore` persists the bookkeeping needed by the blocks-only storage
path. All four backends (memory, badger, sqlite, postgres) implement it and pass
the corresponding conformance group. (There is no separate local-chunk-index
metadata contract: the local journal owns its own `(payloadID, offset)`-keyed
byte cache internally, and the per-file **FileChunk manifest** — written by the
carve `BlockSink` — is the only metadata record of a carved chunk.)

#### BlockRecordStore

Tracks each packed remote block object: its content hash, byte length, live
chunk count, and sync state. The carver uses this to decide which blocks are
safe to upload; the GC uses it to decide which may be deleted.

```go
// pkg/block/block_record.go
type BlockRecord struct {
    BlockID        string
    BlockHash      block.ContentHash
    Length         int64
    LiveChunkCount uint32
    SyncState      block.BlockState // block.BlockStatePending = 0 while uploading
}
```

```go
// pkg/metadata/block_record_store.go
type BlockRecordStore interface {
    // PutBlockRecord writes or overwrites the record for rec.BlockID.
    PutBlockRecord(ctx context.Context, rec block.BlockRecord) error

    // GetBlockRecord retrieves the record for blockID.
    // Returns (_, false, nil) when no record exists — absence is not an error.
    GetBlockRecord(ctx context.Context, blockID string) (block.BlockRecord, bool, error)

    // DeleteBlockRecord removes the record for blockID. Idempotent.
    DeleteBlockRecord(ctx context.Context, blockID string) error

    // WalkBlockRecords calls fn for every stored block record.
    // Returns the first non-nil error from fn or from the store iterator.
    WalkBlockRecords(ctx context.Context, fn func(block.BlockRecord) error) error

    // DecrLiveChunkCount atomically decrements LiveChunkCount for blockID
    // by delta, flooring at 0. Returns the remaining count.
    // Returns an error if blockID does not exist.
    DecrLiveChunkCount(ctx context.Context, blockID string, delta uint32) (remaining uint32, err error)
}
```

Conformance: `storetest` group **BlockRecordOps** covers put/get round-trip,
missing-key (returns `false`, not an error), delete idempotency, walk, and
`DecrLiveChunkCount` with floor clamping.

#### DefaultCommitBlock — one fsync per block

`metadata.DefaultCommitBlock` atomically commits an entire block's metadata
in a single transaction — one fsync per block, not one per chunk:

```go
// pkg/metadata/block_record_store.go
func DefaultCommitBlock(
    ctx context.Context,
    s Transactor,
    rec block.BlockRecord,
    chunks []block.BlockChunkCommit,
    fileChunks []*block.FileChunk,
) error
```

`BlockChunkCommit` bundles the per-chunk commit data:

```go
// pkg/block/block_record.go
type BlockChunkCommit struct {
    Hash   block.ContentHash
    Remote block.ChunkLocator // remote locator recorded via MarkSynced
}
```

Everything commits in a **single** `WithTransaction`, so either the whole block
is visible or none of it is — a commit error just propagates to the caller,
whose requeue logic re-drives the batch. Inside the transaction,
`DefaultCommitBlock`:

1. Calls `GetBlockRecord` — if the block record already exists the whole
   function is a no-op (idempotent restart path; no double-counting, locators
   untouched).
2. Calls `PutBlockRecord` with `rec`.
3. Writes each per-file **FileChunk manifest** row in `fileChunks` — the carver
   passes one per chunk (`ID = {payloadID}/{offset}`, `Hash`, `DataSize`);
   legacy callers pass `nil` and write no rows.
4. Records every chunk's synced marker + remote locator. Locator writes are
   last-wins (delete-then-mark inside the tx), so the cas→blocks migration can
   rewrite a standalone locator to point into the new block.

Backends expose `CommitBlock` on the `Store` interface and SHOULD delegate to
`DefaultCommitBlock`:

```go
CommitBlock(ctx context.Context, rec block.BlockRecord, chunks []block.BlockChunkCommit) error
```

Conformance: group **CommitBlockOps** covers full commit with multiple chunks,
idempotent re-commit, and `MarkSynced` retry after a simulated mid-commit crash.

### Engine API surface

Custom block-store implementations that compose into `*engine.BlockStore`
do not see the engine API directly — that surface is consumed by
adapters via `internal/adapter/common/`. For reference, the signatures
are:

```go
ReadAt(ctx, payloadID, blocks []BlockRef, dest []byte, offset uint64) (int, error)
WriteAt(ctx, payloadID, currentBlocks []BlockRef, data []byte, offset uint64) ([]BlockRef, error)
Truncate(ctx, payloadID, currentBlocks []BlockRef, newSize uint64) ([]BlockRef, error)
Delete(ctx, payloadID, blocks []BlockRef) error
CopyPayload(ctx, srcPayloadID, srcBlocks []BlockRef, dstPayloadID) ([]BlockRef, error)
```

`blocks` is the CAS path with end-to-end BLAKE3 verification.

## The journal (local tier)

The journal is **not a pluggable store**. Every share gets one, provisioned
automatically under the server-level `blockstore.journal.path`, and there is no
type to choose and nothing to register. This section describes the seam so that
code composing an `*engine.BlockStore` — or a test double standing in for the
journal — gets the contract right.

### The LocalStore interface

`pkg/block/local.LocalStore` is the internal contract the journal satisfies. It
is the journal's own vocabulary — the surface is keyed by `(FileID, offset)`,
**not** by content hash. See `pkg/block/local/local.go` for the authoritative
definition and per-method contract; the representative methods are:

```go
type LocalStore interface {
    // --- Data plane (FileID + offset keyed) ---
    WriteAt(ctx context.Context, id journal.FileID, offset int64, data []byte) error
    ReadAt(ctx context.Context, id journal.FileID, offset int64, dst []byte) (n int, st journal.ReadState, err error)
    Hydrate(ctx context.Context, id journal.FileID, offset int64, data []byte, notAfter uint64) error
    WriteVersion() uint64
    Invalidate(ctx context.Context, id journal.FileID, offset, length int64) error
    Commit(ctx context.Context, id journal.FileID) error   // fsync buffered writes
    FileSize(ctx context.Context, id journal.FileID) (int64, bool)
    DataExtents(ctx context.Context, id journal.FileID, fileSize int64) ([][2]uint64, error)
    Truncate(ctx context.Context, id journal.FileID, newSize int64) error
    Delete(ctx context.Context, id journal.FileID) error
    ListFiles(ctx context.Context) []journal.FileID

    // --- Flush (journal → block store) ---
    Flush(ctx context.Context, id journal.FileID, opts journal.FlushOptions, fn journal.FlushFunc) error
    UnsyncedBytes() int64

    // --- Eviction ---
    Evict(ctx context.Context, targetBytes int64) (journal.EvictResult, error)
    SetEvictionEnabled(enabled bool)
    SetEvictionPinned(pinned bool)
    // ... plus lifecycle (Start, Close), Durable/SetDurable, Stats, Closed.
}
```

A write buffers a dirty range and local-acks without fsync; `Commit` is the
durability point. `ReadAt` reports both evicted (`ReadState.Cold`) and uncovered
(`ReadState.Hole`) ranges; the engine reconciles either against the FileChunk
manifest and `Hydrate`s whatever the manifest says the block store holds.

### The Flush seam replaced SetCarveTargets / Carve

Earlier releases wired the offload path by injecting a deduper and a sink into
the store (`SetCarveTargets`) and then asking it to carve (`Carve`). Both are
gone. The seam is now a single method, `Flush`, and everything it needs arrives
as arguments:

```go
Flush(ctx, id, journal.FlushOptions{Force: true, AfterFile: reap}, fn)
```

- `fn` (a `journal.FlushFunc`) is **mandatory**. The journal offers it each
  contiguous dirty run for one file and flips the fragments `fn` reports durable.
  Building it is the caller's job — `pkg/block/engine/flush_closure.go` is the
  production one — and it must be built **fresh per call**; a closure is not
  reusable across flushes.
- `opts.Force` bypasses the age/size batching gate.
- `id` scopes the pass to one file. The empty id is **not** special: a caller
  wanting every file enumerates `ListFiles` and flushes each id in turn.
- `opts.AfterFile` is the per-file reap hook, run once the file's runs are done.

The store no longer holds a deduper or a sink of its own, so a double that
implements `LocalStore` needs no setup call before a flush — and a `Flush` with
a nil `fn` is an error, not a no-op.

### Flushing while the block store is unhealthy

`RemoteSync.Flush` treats an unhealthy block store as a **soft** condition: it
returns `FlushResult{Finalized: false}` with a nil error and leaves the dirty
state for the periodic uploader, rather than surfacing every timeout as a wire
error. Callers must not tight-loop retry on it — surface it and let the client
re-drive on its own schedule.

What changed is what the COMMIT seam does with that soft result when the local
tier is **volatile** (a journal explicitly marked `{"durable": false}`, or the
in-memory store used by tests) and the share asks for `commit_ack: block-store`.
It used to acknowledge anyway; it now returns a hard error (`ErrNotDurableYet`,
normalized to the I/O-class wire code). That is deliberate and more honest: with a
volatile local tier and an unreachable block store, nothing holding the bytes
survives a crash, so acknowledging would be a promise the server cannot keep. With
an ordinary durable journal the soft result is still acked — the bytes already
survive a restart, and the syncer keeps retrying.

### Reference implementations

`*journal.Store` (`pkg/block/journal/`) IS the per-file byte cache the
composition layer holds directly — no adapter between them. The in-memory store
(`pkg/block/local/memory/`) is the other implementation, for tests. There is no
separate append-log or rollup tier and no `metadata.RollupStore` contract — those
were removed when the journal replaced the two-tier local design.

### Conformance tests

The journal-native `LocalStore` surface is `(FileID, offset)`-keyed, so the one
`blockstoretest` suite (`RemoteBlockStoreConformance`) does **not** apply to it — that
suite targets the block-keyed `remote.RemoteBlockStore` surface (see
[Implementing a Remote Block Store](#implementing-a-remote-block-store-block-keyed)).
The journal and the in-memory local store are exercised by their own package tests under
`pkg/block/journal/` and `pkg/block/local/`.

## Implementing a Remote Store

Remote stores provide durable block storage shared across shares via ref counting.

### The RemoteStore Interface

The `pkg/block/remote.RemoteStore` interface defines the contract. Its surface is
entirely block-keyed: packed block objects under the `blocks/` prefix, read and
written through `RemoteBlockStore`, plus the per-chunk `ChunkReader` /
`ChunkSealer` transform seam and the lifecycle/health probes. See
`pkg/block/remote/remote.go` for the authoritative definition:

```go
type RemoteStore interface {
    RemoteBlockStore // PutBlock/GetBlock/GetBlockRange/DeleteBlock/WalkBlocks
    ChunkReader      // ReadChunk: one chunk's plaintext out of a block object
    ChunkSealer      // SealChunk: one chunk's plaintext into its wire bytes

    // HealthCheck is the legacy error-returning probe used by the syncer.
    HealthCheck(ctx context.Context) error

    // Healthcheck returns a structured health.Report (satisfies health.Checker).
    Healthcheck(ctx context.Context) health.Report

    // Close releases resources.
    Close() error
}
```

There is no hash-keyed CAS operation on this surface, and no method on it
verifies content: `ReadChunk` returns chunk plaintext and the engine recomputes
its BLAKE3 afterwards (`readChunkVerified` in `pkg/block/engine/fetch.go`),
because no single decorator layer holds both the wire bytes and the
plaintext-hash domain. Per-method semantics are documented under
[Implementing a Remote Block Store](#implementing-a-remote-block-store-block-keyed);
implement that interface and the three methods above.

### Ref Counting

Remote stores are shared across shares via ref counting:
- When a share is created referencing a remote store, the ref count increments
- When a share is removed, the ref count decrements
- When the ref count reaches zero, `Close()` is called

This means your `Close()` implementation should release all resources (connections, goroutines, etc.).

### Reference Implementation

See `pkg/block/remote/s3/` for a production S3 remote store implementation with:
- Configurable retry with exponential backoff
- Health check via HEAD bucket
- Efficient multipart uploads for large blocks

### Conformance Tests

There is no conformance suite for the legacy CAS surface — it was deleted along with
the `block.Store` interface. The only suite is `RemoteBlockStoreConformance`, covered
under [Implementing a Remote Block Store](#implementing-a-remote-block-store-block-keyed).

## Implementing a Remote Block Store (block-keyed)

The `pkg/block/remote.RemoteBlockStore` interface is the **block-keyed** (non-CAS) remote store surface used by the live write path. Every new write is packed into block objects under the `blocks/` prefix, separate from the legacy CAS `cas/` namespace — the two namespaces never collide. New remote backends should implement `RemoteBlockStore`.

> **Legacy CAS objects:** `cas/<hash>` objects written before the blocks-only flip are no longer reachable — the hash-keyed accessors that read them were removed along with the `block.Store` interface, and no backend carries them any more. A deployment holding pre-flip data must be migrated before upgrading; see [architecture.md](architecture.md) for the consequences of skipping that. A new backend implements `RemoteBlockStore` only.

### The RemoteBlockStore Interface

```go
type RemoteBlockStore interface {
    PutBlock(ctx context.Context, blockID string, r io.Reader) error
    GetBlock(ctx context.Context, blockID string) ([]byte, error)
    GetBlockRange(ctx context.Context, blockID string, offset, length int64) ([]byte, error)
    DeleteBlock(ctx context.Context, blockID string) error
    WalkBlocks(ctx context.Context, fn func(blockID string, meta block.Meta) error) error
}
```

See `pkg/block/remote/remote.go` for the authoritative definition and per-method docstrings. Current implementations: `pkg/block/remote/s3/` and `pkg/block/remote/memory/`.

#### Key shape

Objects are keyed by an opaque `blockID` string. The on-disk/on-wire key is `block.FormatBlockKey(blockID)` = `"blocks/<blockID>"`. Backends derive this key internally; callers and the engine never construct raw S3/object-store keys.

#### PutBlock

Writes the content of `r` under `blocks/<blockID>`. Implementations **must** stream `r` without buffering the full body — callers may supply an unbounded reader (e.g., an in-progress packing file). The call is idempotent: a second `PutBlock` for the same `blockID` overwrites silently.

#### GetBlock

Returns the full bytes of the named block object. Returns `block.ErrChunkNotFound` when the block is absent. The returned slice must be freshly allocated and must not alias internal storage.

#### GetBlockRange

Returns `[offset, offset+length)` bytes of the block object. Bounds validation:

- **Negative offset** — must return `block.ErrInvalidOffset` (client-validated before any network call).
- **Past-EOF offset** — a past-EOF offset cannot be detected here without a `HEAD`, so backends surface a native error (S3: HTTP 416) rather than `ErrInvalidOffset`. The contract only requires _some_ error for `offset >= EOF`.
- **Non-positive length** — must return `block.ErrInvalidSize`.
- **Past-EOF length** — clamped to the object's remaining bytes on backends that support partial-content (S3 `Range` header); no error.
- **Absent blockID** — must return `block.ErrChunkNotFound`.

#### DeleteBlock

Removes the block object. Idempotent: deleting an absent `blockID` must return `nil`.

#### WalkBlocks

Enumerates every block object under the `blocks/` prefix. The callback receives the `blockID` (with the `blocks/` prefix stripped) and a `block.Meta` (size, last-modified timestamp). Ordering is unspecified.

- Returning `block.ErrStopWalk` from the callback is a clean early exit — `WalkBlocks` returns `nil`.
- Any other callback error halts enumeration and is returned wrapped as `"walk halted at <blockID>: %w"`.
- Context cancellation aborts immediately.

### Block codec wire format

Block objects are written by `pkg/block/blockcodec`. A single block packs one or more chunk records into a continuous byte stream:

```
Block = [ preamble ][ record_0 ][ record_1 ] … [ record_{N-1} ]

preamble:
  magic      [4]byte = {'D','F','B','1'}   // "DFB1" — identifies a DittoFS block object
  flags      uint8                          // bit0 = 1 → record headers are AEAD-sealed
  blockID    uvarint(len) + len bytes (UTF-8)

record (plaintext, flags bit0 = 0):
  hash       [32]byte                       // chunk BLAKE3 content hash
  wireLen    uvarint
  wire       [wireLen]byte                  // enc(comp(chunk)) — already-transformed body

record (sealed, flags bit0 = 1):
  sealedHdrLen  uvarint
  sealedHdr     [sealedHdrLen]byte          // AEAD seal of {hash[32], wireLen uvarint}
                                            // AAD = blockID || recordIndex(uvarint)
  wire          [wireLen]byte               // wireLen recovered by opening sealedHdr
```

**Sealed-header framing** is used on encrypted shares. The AEAD tag covers `hash + wireLen` with `blockID||recordIndex` as additional authenticated data, while the `wire` body is already the encrypted chunk body (the encryption decorator applies its transform before `Builder.Add` is called). The chunk metadata is therefore authenticated even though the wire body is opaque.

**Locators.** Each call to `Builder.Add(hash, wire)` returns a `block.ChunkLocator{BlockID, WireOffset, WireLength}` — the byte range of the wire body within the block object. `WireOffset` and `WireLength` are over wire bytes (after the per-record header), not plaintext bytes. The engine stores these locators in the metadata store and recovers an individual chunk via `GetBlockRange(blockID, WireOffset, WireLength)` (surfaced through the optional `ChunkReader.ReadChunk` extension on the same backend).

### Conformance suite

Every new `RemoteBlockStore` backend must pass `blockstoretest.RemoteBlockStoreConformance`:

```go
package myremote_test

import (
    "testing"
    "github.com/marmos91/dittofs/pkg/block/blockstoretest"
)

func TestMyRemoteBlockStore(t *testing.T) {
    factory := func(t *testing.T) (blockstoretest.RemoteBlockStore, func()) {
        store, cleanup := createTestStore(t)
        return store, cleanup
    }
    blockstoretest.RemoteBlockStoreConformance(t, factory)
}
```

The suite covers: `PutBlock`/`GetBlock` round-trip, no-aliasing, `GetBlockRange` mid-range/past-EOF-clamped/invalid-offset/invalid-size/absent, `DeleteBlock` durability and idempotency, `WalkBlocks` enumeration and `ErrStopWalk`, idempotent `PutBlock`, zero-byte blocks, and concurrent same-ID `PutBlock`.

## Best Practices

### Thread Safety

All store implementations must be thread-safe. Multiple goroutines will access the store concurrently.

### Context Handling

Always respect context cancellation, especially for remote stores where network calls can be slow:

```go
func (s *MyStore) ReadBlock(ctx context.Context, blockID string) ([]byte, error) {
    if err := ctx.Err(); err != nil {
        return nil, err
    }
    // Proceed with operation
}
```

### Error Handling

- Journal errors should be wrapped with meaningful context
- Block store errors should distinguish transient (retry-able) from permanent failures
- Delete operations should be idempotent (deleting a non-existent block is not an error)

### Performance

- **Block stores**: Use connection pooling, implement retry with backoff, batch operations where possible

## Testing Your Implementation

1. **Conformance tests**: Run the provided test suite (`pkg/block/blockstoretest`)
2. **Concurrency tests**: Verify thread safety with parallel reads/writes
3. **Error handling tests**: Test behavior with canceled contexts, network failures
4. **Integration tests**: Test with the full DittoFS stack (create share, mount, read/write)

## Common Pitfalls

1. **Not making Delete idempotent**: Deleting a non-existent block should succeed
2. **Ignoring context cancellation**: Long operations should check `ctx.Err()` periodically
3. **Unsafe concurrent access**: Use proper synchronization for shared state
4. **Resource leaks**: Ensure `Close()` releases all resources (connections, goroutines, file handles)

## Integration with DittoFS

### Register Your Store

Add your store type to the block-store factory, and to the type whitelist the
REST layer validates against:

```go
// pkg/controlplane/runtime/shares/blockstore_config.go
func CreateRemoteStoreFromConfig(ctx context.Context, storeType string, cfg …) (remote.RemoteStore, error) {
    switch storeType {
    case "memory":
        return remotememory.New(), nil
    case "s3":
        // …
    case "myremote":
        return myremote.New(config)
    default:
        return nil, fmt.Errorf("unknown block store type: %s", storeType)
    }
}

// internal/controlplane/api/handlers/block_stores.go
func validateBlockStoreType(storeType string) bool {
    return storeType == "s3" || storeType == "memory" || storeType == "myremote"
}
```

There is no kind to register alongside the type: a block store row is unique by
name alone.

### Selecting the store

Create it with `dfsctl store block add --name <name> --type <type>` and attach it
to a share with `dfsctl share create/edit --block-store <name>`. The journal has
no equivalent — it is not a named entity the CLI can create, and the server builds
each share's from `blockstore.journal.*` in the server config.

## Additional Resources

- **Interface Definitions**: `pkg/block/local/local.go`, `pkg/block/remote/remote.go`
- **Reference Implementations**:
  - Journal (local tier): `pkg/block/journal/`, `pkg/block/local/memory/`
  - Block stores: `pkg/block/remote/s3/`, `pkg/block/remote/memory/`
  - Metadata: `pkg/metadata/store/memory/`, `pkg/metadata/store/badger/`, `pkg/metadata/store/sqlite/`, `pkg/metadata/store/postgres/` (the SQL pair share `pkg/metadata/store/sql/`)
- **Conformance Tests**: `pkg/block/blockstoretest/` (block stores), `pkg/metadata/storetest/` (metadata stores)
- **Architecture**: `docs/ARCHITECTURE.md`
- **Configuration**: `docs/CONFIGURATION.md`
- **Contributing**: `docs/CONTRIBUTING.md`
