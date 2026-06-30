# Implementing Custom Stores

This guide provides comprehensive instructions for implementing custom metadata stores, local block stores, and remote block stores for DittoFS. Whether you're building a database-backed metadata store or a custom cloud storage integration, this document will walk you through the process with best practices and practical examples.

## Table of Contents

1. [Overview](#overview)
2. [When to Implement Custom Stores](#when-to-implement-custom-stores)
3. [Understanding the Architecture](#understanding-the-architecture)
4. [Implementing Metadata Stores](#implementing-metadata-stores)
5. [Implementing a Local Store](#implementing-a-local-store)
6. [Implementing a Remote Store](#implementing-a-remote-store)
7. [Implementing a Remote Block Store (block-keyed)](#implementing-a-remote-block-store-block-keyed)
8. [Best Practices](#best-practices)
9. [Testing Your Implementation](#testing-your-implementation)
10. [Common Pitfalls](#common-pitfalls)
11. [Integration with DittoFS](#integration-with-dittofs)

## Overview

DittoFS uses a **two-tier block store architecture** with three distinct store types:

- **Metadata Stores**: Simple CRUD operations for file/directory structure, attributes, permissions
- **Local Block Stores**: Fast, per-share storage on local disk or in memory (L2 cache tier)
- **Remote Block Stores**: Durable storage in S3 or compatible object stores (L3 tier, shared across shares via ref counting)

**Key Design Principle**: Each share gets its own `*engine.BlockStore` instance that composes a local store, optional remote store, and syncer. The engine orchestrates reads and writes across the tiers. Local stores provide fast access; remote stores provide durability.

This separation enables:
- Independent scaling of metadata and block storage
- Per-share isolation with shared remote backends
- Different storage tiers (hot/cold storage, SSD/HDD)
- Simple store implementations (just implement the interface, the engine handles coordination)

## When to Implement Custom Stores

### Metadata Store Use Cases

Implement a custom metadata store when you need:

- **Database-backed storage**: PostgreSQL, MySQL, MongoDB, Cassandra
- **Distributed metadata**: Multi-node coordination, consensus protocols
- **Advanced features**: Full-text search, custom indexing, complex queries
- **Compliance**: Audit logs, versioning, immutability guarantees

**Example**: A PostgreSQL-backed metadata store for enterprise environments requiring audit trails and high availability.

### Local Store Use Cases

Implement a custom local store when you need:

- **Specialized local storage**: NVMe-optimized, hardware-accelerated compression
- **Custom eviction**: Access-pattern-aware eviction beyond simple LRU
- **Encryption at rest**: Hardware-accelerated encryption for local blocks

**Reference implementation**: `pkg/block/local/fs/` (filesystem-backed local store)

### Remote Store Use Cases

Implement a custom remote store when you need:

- **Cloud storage integration**: Azure Blob, Google Cloud Storage, custom object stores
- **Specialized storage**: Tape archives, HSM systems, data lakes
- **Tiering**: Automatic hot/cold data movement based on access patterns

**Reference implementation**: `pkg/block/remote/s3/` (S3-backed remote store)

## Understanding the Architecture

### Per-Share BlockStore

Each share gets its own `*engine.BlockStore` instance:

```
┌─────────────────────────────────────┐
│  engine.BlockStore (per-share)      │
│                                     │
│  ┌─────────────┐  ┌─────────────┐  │
│  │ LocalStore  │  │ RemoteStore │  │
│  │ (required)  │  │ (optional)  │  │
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

- **LocalStore** is required -- all reads and writes go through local storage first
- **RemoteStore** is optional -- when configured, the Syncer asynchronously uploads local blocks to remote storage
- **Ref counting**: Remote stores are shared across shares; when the last share using a remote store is removed, the connection is closed

### File Handle and Block Resolution

Protocol handlers resolve the per-share block store via `GetBlockStoreForHandle(ctx, handle)`:

1. File handle encodes the share name
2. Runtime extracts share name and returns the share's BlockStore
3. Handler calls `ReadAt` / `WriteAt` on the BlockStore

## Implementing Metadata Stores

The metadata store interface and implementation guide remains the same as before. See the `pkg/metadata/Store` interface and reference implementations:

- `pkg/metadata/store/memory/`: In-memory (fast, ephemeral)
- `pkg/metadata/store/badger/`: BadgerDB (persistent, embedded)
- `pkg/metadata/store/postgres/`: PostgreSQL (persistent, distributed)

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
`ListFileChunks(ctx, payloadID)` for the engine's hot paths. All three built-in
backends (memory, badger, postgres) satisfy it without changes — the
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
   batch. Runs against all three built-in backends and any custom backend
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

### Block layout flag

Metadata backends MUST persist a `block_layout` field on the share
record. The field is a `metadata.BlockLayout` enum (string-shaped on
the wire) with values `legacy` or `cas-only`.

```go
// pkg/metadata/types.go
type BlockLayout uint8

const (
    BlockLayoutLegacy   BlockLayout = iota   // pre-migration on-disk layout
    BlockLayoutCASOnly                       // CAS-only: legacy reads fail loud
)

type ShareOptions struct {
    // ... pre-existing fields ...
    BlockLayout BlockLayout
}
```

**Forward-compat rule:** Empty / missing values MUST coerce to
`BlockLayoutLegacy` on read so pre-Phase-14 metadata rows decode
cleanly. Use the `metadata.ParseBlockLayout("")` helper, which
returns `BlockLayoutLegacy`. Unknown values (anything other than
`legacy` / `cas-only`) MUST surface `metadata.ErrInvalidBlockLayout`
rather than coercing — silent coercion would mask bugs in upstream
backends.

**Round-trip invariant:** `UpdateShareOptions(BlockLayout=cas-only)`
followed by `GetShare(name)` MUST observe `cas-only`. The migration
tool's cutover (D-A7) depends on this txn being durable AND visible
to the engine's next share-open.

The conformance suite scenario `RunBlockLayoutSuite` exercises
round-trip and update semantics. New backends MUST invoke it from
their per-backend test file:

```go
import "github.com/marmos91/dittofs/pkg/metadata/storetest"

func TestBlockLayoutConformance(t *testing.T) {
    storetest.RunBlockLayoutSuite(t, factoryFunc)
}
```

The suite asserts:

1. **Default is `legacy`** for newly created shares (pre-existing
   shares upgraded into v0.15+ default to `legacy`).
2. **Round-trip** of `UpdateShareOptions(BlockLayout=cas-only)` →
   `GetShare()` returns `cas-only`.
3. **Empty-string coercion** at the parsing boundary
   (`ParseBlockLayout("") == BlockLayoutLegacy`).
4. **Unknown-value rejection** via `ErrInvalidBlockLayout`.
5. **Atomic update** semantics — concurrent updates of the same
   share's `BlockLayout` serialize through the backend's existing
   share-update path (D-A7 piggybacks on this).

**Recommended persistence shape per backend:**

| Backend  | Recommended layout                                                   |
|----------|----------------------------------------------------------------------|
| Postgres | Dedicated `block_layout TEXT NOT NULL DEFAULT 'legacy'` column on `shares`. Authoritative over the options JSON blob. |
| Badger   | Inline-encoded inside the existing `ShareOptions` blob (gob; `omitempty` on the field for forward-compat with older rows). |
| Memory   | Direct field on the in-process struct; no persistence layer.         |

Cross-references:

- [ARCHITECTURE.md — Migration & Block-Layout Routing](architecture.md#migration--block-layout-routing)
  for how the engine consumes the flag.
- [BLOCKSTORE_MIGRATION.md](../guide/block-store-migration.md)
  for the operator-facing migration story.

### Block Record and Local Chunk Index

Two additional metadata interfaces persist the bookkeeping needed by the
blocks-only storage path. All four backends (memory, badger, sqlite, postgres)
implement both and pass the corresponding conformance groups.

#### BlockRecordStore

Tracks each log-blob block object: its content hash, byte length, live chunk
count, and sync state. The syncer uses this to decide which blocks are safe to
upload; the GC uses it to decide which may be deleted.

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

#### LocalChunkIndex

Maps a chunk's content hash to its position in a local log-blob file. It is
the local-tier analog of `SyncedHashStore`: both are keyed by `ContentHash`,
both carry a physical locator, and both follow the same idempotent-put /
safe-miss-on-get / idempotent-delete contract.

| Interface         | Key           | Value                        | Tier   |
|-------------------|---------------|------------------------------|--------|
| `SyncedHashStore` | `ContentHash` | `block.ChunkLocator`         | Remote |
| `LocalChunkIndex` | `ContentHash` | `block.LocalChunkLocation`   | Local  |

```go
// pkg/block/block_record.go
type LocalChunkLocation struct {
    LogBlobID string // blob filename stem, e.g. "0000000000000001"
    RawOffset int64  // byte offset within that blob
    RawLength int64  // byte length of the raw chunk
}
```

```go
// pkg/metadata/block_record_store.go
type LocalChunkIndex interface {
    // PutLocalLocation records or overwrites the local position for hash.
    PutLocalLocation(ctx context.Context, hash block.ContentHash, loc block.LocalChunkLocation) error

    // GetLocalLocation returns the local position for hash.
    // Returns (_, false, nil) when no entry exists.
    GetLocalLocation(ctx context.Context, hash block.ContentHash) (block.LocalChunkLocation, bool, error)

    // DeleteLocalLocation removes the local position for hash. Idempotent.
    DeleteLocalLocation(ctx context.Context, hash block.ContentHash) error
}
```

Conformance: group **LocalIndexOps** covers put/get round-trip, upsert
overwrites (second write wins), missing-key, and delete idempotency.

#### DefaultCommitBlock — one fsync per block

`metadata.DefaultCommitBlock` atomically commits an entire block's metadata
in a single transaction — one fsync per block, not one per chunk:

```go
// pkg/metadata/block_record_store.go
func DefaultCommitBlock(
    ctx context.Context,
    s interface {
        Transactor
        SyncedHashStore
    },
    rec block.BlockRecord,
    chunks []block.BlockChunkCommit,
) error
```

`BlockChunkCommit` bundles the per-chunk commit data:

```go
// pkg/block/block_record.go
type BlockChunkCommit struct {
    Hash   block.ContentHash
    Remote block.ChunkLocator      // remote locator passed to MarkSynced
    Local  block.LocalChunkLocation // local locator stored in LocalChunkIndex
}
```

Inside `WithTransaction`, `DefaultCommitBlock`:

1. Calls `GetBlockRecord` — if the block record already exists the whole
   function is a no-op (idempotent restart path; no double-counting).
2. Calls `PutBlockRecord` with `rec`.
3. Calls `PutLocalLocation` for every chunk in `chunks`.

After the transaction commits, it calls `MarkSynced` on `SyncedHashStore`
for every chunk, recording the remote locator. `MarkSynced` runs outside
the transaction and is itself idempotent, so a crash between the committed
transaction and the last `MarkSynced` call is safe: the next retry skips the
transaction (block record already exists) and re-runs only the `MarkSynced`
loop.

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

## Implementing a Local Store

Local stores provide fast, per-share block storage. Each share gets an isolated local storage directory.

### The LocalStore Interface

The `pkg/block/local.LocalStore` interface defines the contract. Storage is
**content-addressed** — chunks are keyed by their BLAKE3 `block.ContentHash`,
not by a block ID + offset. The interface embeds the content-addressed
`block.BlockStoreAppend` surface and adds lifecycle, per-payload admin,
rollup-drain, and retention methods. See `pkg/block/local/local.go` for the
authoritative definition; the embedded CAS contract is:

```go
type Store interface { // block.Store, embedded by LocalStore via BlockStoreAppend
    // Put writes data under the key derived from hash (idempotent for
    // identical bytes). Returns an error on a zero hash.
    Put(ctx context.Context, hash ContentHash, data []byte) error

    // Get returns the chunk bytes addressed by hash (freshly allocated,
    // never aliasing internal storage). Returns ErrChunkNotFound if absent.
    Get(ctx context.Context, hash ContentHash) ([]byte, error)

    // GetRange returns the byte sub-range [offset, offset+length).
    GetRange(ctx context.Context, hash ContentHash, offset, length int64) ([]byte, error)

    // Has reports whether the store holds the object addressed by hash.
    Has(ctx context.Context, hash ContentHash) (bool, error)

    // Delete removes the object addressed by hash.
    Delete(ctx context.Context, hash ContentHash) error

    // Head returns object metadata (size, last-modified) without the body.
    Head(ctx context.Context, hash ContentHash) (ObjectInfo, error)

    // Walk enumerates every stored object. Return ErrStopWalk to exit early.
    Walk(ctx context.Context, fn func(ObjectInfo) error) error
}
```

`BlockStoreAppend` adds `AppendWrite` and `DeleteAppendLog` for the append-log
write path. `LocalStore` then layers on lifecycle (`Start`, `Close`),
per-payload admin (`Truncate`, `EvictMemory`, `GetFileSize`, `ListFiles`,
`ReadPayloadAt`), snapshot drain/reset (`DrainRollups`, `ResetLocalState`),
and retention policy (`SetRetentionPolicy`, `SetEvictionEnabled`) — consult
`pkg/block/local/local.go` for the full method set and per-method contract.

### Reference Implementation

See `pkg/block/local/fs/` for a complete filesystem-backed local store implementation that handles:
- Per-share isolated directories
- Atomic writes
- Block listing for sync operations

### Append-log + CAS chunk tier

The local filesystem store (`*fs.FSStore`) writes through an append-only
log per file and rolls it up into content-addressed (CAS) chunks. The
chunk-tier methods on `*fs.FSStore` are: `AppendWrite`, `StoreChunk`,
`ReadChunk`, `HasChunk`, `DeleteChunk`, `DeleteAppendLog`,
`TruncateAppendLog`, and `StartRollup`.

A per-file metadata surface `metadata.RollupStore` (two methods:
`SetRollupOffset`, `GetRollupOffset`) persists the log's `rollup_offset`.
The built-in memory, Badger, and Postgres backends all implement it. New
metadata backends must add equivalent persistence keyed by `payloadID`,
backed by an atomic upsert (metadata is the source of truth for
`rollup_offset`; see `docs/ARCHITECTURE.md`).

### Conformance Tests

Test your local store with the conformance suite:

```go
package mylocal_test

import (
    "testing"

    "github.com/marmos91/dittofs/pkg/block"
    "github.com/marmos91/dittofs/pkg/block/blockstoretest"
)

func TestMyLocalStore(t *testing.T) {
    // The factory returns a fresh store plus a cleanup closure per subtest.
    factory := func(t *testing.T) (block.Store, func()) {
        store, cleanup := createTestStore(t)
        return store, cleanup
    }
    blockstoretest.BlockStoreConformance(t, factory)

    // If your store implements the append-write absorber (block.BlockStoreAppend),
    // also run the append conformance suite:
    appendFactory := func(t *testing.T) (block.BlockStoreAppend, func()) {
        store, cleanup := createTestAppendStore(t)
        return store, cleanup
    }
    blockstoretest.BlockStoreAppendConformance(t, appendFactory)
}
```

## Implementing a Remote Store

Remote stores provide durable block storage shared across shares via ref counting.

### The RemoteStore Interface

The `pkg/block/remote.RemoteStore` interface defines the contract. Like the
local store, remote storage is **content-addressed**: the interface embeds the
CAS `block.Store` surface (`Put`, `Get`, `GetRange`, `Has`, `Delete`, `Head`,
`Walk` — keyed by `block.ContentHash`) and adds verification + health methods.
See `pkg/block/remote/remote.go` for the authoritative definition:

```go
type RemoteStore interface {
    block.Store // CAS Put/Get/GetRange/Has/Delete/Head/Walk

    // ReadBlockVerified GETs the object addressed by hash and verifies the
    // body's BLAKE3 hash matches `expected` before returning bytes. Returns
    // block.ErrCASContentMismatch on any verification failure.
    ReadBlockVerified(ctx context.Context, hash block.ContentHash, expected block.ContentHash) ([]byte, error)

    // HealthCheck is the legacy error-returning probe used by the syncer.
    HealthCheck(ctx context.Context) error

    // Healthcheck returns a structured health.Report (satisfies health.Checker).
    Healthcheck(ctx context.Context) health.Report

    // Close releases resources.
    Close() error
}
```

### Implementation Pattern

```go
package myremote

import (
    "context"

    "github.com/marmos91/dittofs/pkg/block"
)

type MyRemoteStore struct {
    client *MyCloudClient
    bucket string
}

func New(config Config) (*MyRemoteStore, error) {
    client, err := connectToCloud(config)
    if err != nil {
        return nil, err
    }
    return &MyRemoteStore{client: client, bucket: config.Bucket}, nil
}

// objectKey derives the storage key from the content hash. The CAS key is
// the hash; backends typically use its hex form, optionally sharded.
func (s *MyRemoteStore) objectKey(hash block.ContentHash) string {
    return hash.String()
}

func (s *MyRemoteStore) Get(ctx context.Context, hash block.ContentHash) ([]byte, error) {
    // Fetch the chunk addressed by hash. Return block.ErrChunkNotFound if absent.
    return s.client.GetObject(ctx, s.bucket, s.objectKey(hash))
}

func (s *MyRemoteStore) Put(ctx context.Context, hash block.ContentHash, data []byte) error {
    // Upload the chunk under its content-hash key (idempotent for identical bytes).
    return s.client.PutObject(ctx, s.bucket, s.objectKey(hash), data)
}

func (s *MyRemoteStore) Delete(ctx context.Context, hash block.ContentHash) error {
    // Remove the chunk (idempotent).
    err := s.client.DeleteObject(ctx, s.bucket, s.objectKey(hash))
    if err != nil && !isNotFoundError(err) {
        return err
    }
    return nil
}

func (s *MyRemoteStore) HealthCheck(ctx context.Context) error {
    // Verify connectivity (e.g., HEAD bucket)
    return s.client.HeadBucket(ctx, s.bucket)
}

func (s *MyRemoteStore) Close() error {
    return s.client.Close()
}

// ... implement the remaining block.Store methods (GetRange, Has, Head, Walk),
// ReadBlockVerified, and Healthcheck — see pkg/block/remote/s3 for a full example.
```

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

### CAS contracts

All uploads go through a content-addressable keyspace
`cas/{hh}/{hh}/{hex}`, and every byte downloaded from the remote is
verified against the expected BLAKE3 hash. Two contract methods are
required for any RemoteStore implementation that participates in the write
path.

#### RemoteStore.WriteBlockWithHash

```go
// WriteBlockWithHash uploads data under the CAS key derived from h
// and sets a backend-native object-metadata header carrying the hash.
//
// Semantics:
//   - The key MUST be derived from h via FormatCASKey (cas/{hh}/{hh}/{hex}).
//   - The backend-native object metadata MUST set "content-hash" to
//     "blake3:" + hex(h). For S3, this becomes the user-metadata header
//     x-amz-meta-content-hash. For other backends, set the equivalent
//     custom-metadata field.
//   - The PUT MUST be atomic: either the object exists at the CAS key
//     with the correct bytes AND the metadata header, or it does not
//     exist at all.
//   - The call MUST be idempotent: re-uploading the same h with the
//     same bytes is a no-op (or an overwrite that yields identical
//     state). This is what makes the syncer's restart-recovery janitor
//     safe.
//   - Errors are returned as typed values mapped through
//     internal/adapter/common/.
WriteBlockWithHash(ctx context.Context, blockKey string, hash ContentHash, data []byte) error
```

External tooling (e.g. `aws s3api head-object`) MUST be able to verify
the header without DittoFS-specific tooling.

#### RemoteStore.ReadBlockVerified

```go
// ReadBlockVerified reads the object at the CAS key derived from h
// and verifies its bytes against h end-to-end before returning them
// to the caller.
//
// Semantics:
//   - HEAD-style pre-check: if the backend exposes the content-hash
//     header cheaply (S3 GetObject returns it in the same response,
//     so no extra round-trip is needed), reject early with
//     ErrCASContentMismatch when the header does not match h.
//   - Streaming verification: the body is fed to a blake3.Hasher as
//     the caller reads it. On EOF, hasher.Sum(nil) MUST equal h or
//     the call returns ErrCASContentMismatch and the buffer is
//     discarded -- corrupt bytes MUST NOT be surfaced upstream.
//   - The streaming verifier sees bytes once (zero extra allocation).
//   - Verification is hard-required: there is no opt-out knob.
ReadBlockVerified(ctx context.Context, blockKey string, expected ContentHash) ([]byte, error)
```

Header pre-check + streaming recompute is "fail-closed twice" by
design: the header alone is not sufficient (would trust the backend
to never silently corrupt); recompute alone wastes a body read on a
definitively-wrong object.

### Conformance Tests

Test your remote store with the conformance suite:

```go
package myremote_test

import (
    "testing"

    "github.com/marmos91/dittofs/pkg/block"
    "github.com/marmos91/dittofs/pkg/block/blockstoretest"
)

func TestMyRemoteStore(t *testing.T) {
    factory := func(t *testing.T) (block.Store, func()) {
        store, cleanup := createTestStore(t)
        return store, cleanup
    }
    blockstoretest.BlockStoreConformance(t, factory)
}
```

The `BlockStoreConformance` suite pins the CAS contract: `Put` + `Get`
round-trip with no aliasing, idempotent re-`Put` of identical bytes,
`Get`/`GetRange`/`Has`/`Head`/`Delete` semantics, and `Walk` enumeration.
The dedicated `ReadBlockVerified` path on `RemoteStore` (round-trip succeeds;
body-mismatch returns `block.ErrCASContentMismatch`; corrupt bytes never
surface upstream) is exercised separately — see `pkg/block/remote/s3` for the
verification tests.

## Implementing a Remote Block Store (block-keyed)

The `pkg/block/remote.RemoteBlockStore` interface is the **block-keyed** (non-CAS) remote store surface introduced by the blocks-only storage redesign (epic [#1493](https://github.com/marmos91/dittofs/issues/1493)). Block objects live under the `blocks/` prefix, separate from the legacy CAS `cas/` namespace — the two namespaces never collide.

> **Legacy path (being retired):** The per-chunk CAS object path (`cas/<hash>`, hash-keyed `Put`/`Get`/`GetRange` on `RemoteStore`) is being retired in favour of blocks-only storage as part of epic #1493. New remote backends should implement `RemoteBlockStore`. The legacy `RemoteStore` CAS surface will be removed in a later PR of the same epic.

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

- Local store errors should be wrapped with meaningful context
- Remote store errors should distinguish transient (retry-able) from permanent failures
- Delete operations should be idempotent (deleting a non-existent block is not an error)

### Performance

- **Local stores**: Minimize syscalls, use buffered I/O, consider memory-mapped files
- **Remote stores**: Use connection pooling, implement retry with backoff, batch operations where possible

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

Add your store type to the configuration system:

```go
// pkg/config/stores.go
func createLocalStore(config LocalStoreConfig) (local.LocalStore, error) {
    switch config.Type {
    case "fs":
        return fs.New(config.Path)
    case "memory":
        return memory.New()
    case "mylocal":
        return mylocal.New(config.Path)
    default:
        return nil, fmt.Errorf("unknown local store type: %s", config.Type)
    }
}
```

### CLI Integration

Users can then create your store via CLI:

```bash
./dfsctl store block add --kind local --name my-store --type mylocal \
  --config '{"path":"/data/blocks"}'
```

## Additional Resources

- **Interface Definitions**: `pkg/block/local/local.go`, `pkg/block/remote/remote.go`
- **Reference Implementations**:
  - Local: `pkg/block/local/fs/`, `pkg/block/local/memory/`
  - Remote: `pkg/block/remote/s3/`, `pkg/block/remote/memory/`
  - Metadata: `pkg/metadata/store/memory/`, `pkg/metadata/store/badger/`, `pkg/metadata/store/postgres/`
- **Conformance Tests**: `pkg/block/blockstoretest/` (block stores), `pkg/metadata/storetest/` (metadata stores)
- **Architecture**: `docs/ARCHITECTURE.md`
- **Configuration**: `docs/CONFIGURATION.md`
- **Contributing**: `docs/CONTRIBUTING.md`
