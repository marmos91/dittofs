# DittoFS Architecture

This document provides a deep dive into DittoFS's architecture, design patterns, and internal implementation.

**Storage terms used throughout** (see the [Glossary](../guide/glossary.md) for protocol and security terms):

- **CAS** (Content-Addressed Storage) — blocks are named by the hash of their contents rather than by location, so identical data is stored once and deduplicated automatically.
- **FastCDC** — a content-defined chunking algorithm that splits file data at content-based boundaries, so small edits only re-chunk the affected region ([FastCDC paper](https://www.usenix.org/conference/atc16/technical-sessions/presentation/xia)).
- **BLAKE3** — the fast cryptographic hash used to address CAS blocks and verify them end-to-end ([BLAKE3 spec](https://github.com/BLAKE3-team/BLAKE3-specs)).

## Table of Contents

- [Core Abstraction Layers](#core-abstraction-layers)
- [Per-Share Block Store Isolation](#per-share-block-store-isolation)
- [Storage Tiers](#storage-tiers)
- [Adapter Pattern](#adapter-pattern)
- [Control Plane Pattern](#control-plane-pattern)
- [Service Layer](#service-layer)
- [Built-In and Custom Backends](#built-in-and-custom-backends)
- [Directory Structure](#directory-structure)
- [Horizontal Scaling with PostgreSQL](#horizontal-scaling-with-postgresql)
- [Durable Handle State Flow](#durable-handle-state-flow)
- [Engine API + BlockRef + Cache](#engine-api--blockref--cache)
- [File-Level Dedup: ObjectID + Merkle Root](#file-level-dedup-objectid--merkle-root)
- [Migration & Block-Layout Routing](#migration--block-layout-routing)

## Core Abstraction Layers

DittoFS uses a **Runtime-centric architecture** where the Runtime is the single entrypoint for all operations. This design ensures that both persistent store and in-memory state stay synchronized.

```
┌─────────────────────────────────────────┐
│         Protocol Adapters               │
│            (NFS, SMB)                   │
│       pkg/adapter/{nfs,smb}/            │
└───────────────┬─────────────────────────┘
                │ GetBlockStoreForHandle(handle)
                ▼
┌─────────────────────────────────────────┐
│              Runtime                    │
│   (Composition layer + sub-services)    │
│   pkg/controlplane/runtime/             │
│                                         │
│  ┌──────────┐ ┌────────┐ ┌──────────┐  │
│  │ adapters │ │ stores │ │  shares  │  │
│  │lifecycle │ │registry│ │per-share │  │
│  └──────────┘ └────────┘ │BlockStore│  │
│  ┌──────────┐ ┌────────┐ └──────────┘  │
│  │  mounts  │ │lifecycl│ ┌──────────┐  │
│  │ tracking │ │  serve  │ │ identity │  │
│  └──────────┘ └────────┘ │ mapping  │  │
│                           └──────────┘  │
│  ┌────────────┐  ┌───────────────────┐  │
│  │   Store    │  │   Auth Layer      │  │
│  │ (Persist)  │  │   pkg/auth/       │  │
│  │ 9 sub-ifs  │  │ AuthProvider,     │  │
│  │            │  │ IdentityMapper    │  │
│  └────────────┘  └───────────────────┘  │
└───────┬───────────────────┬─────────────┘
        │                   │
        ▼                   ▼
┌────────────────┐  ┌──────────────────────┐
│   Metadata     │  │ Per-Share BlockStore │
│     Stores     │  │  pkg/block/     │
│                │  │                      │
│  - Memory      │  │  ┌──────────────┐    │
│  - BadgerDB    │  │  │ Local Store  │    │
│  - PostgreSQL  │  │  │ fs / memory  │    │
│                │  │  └──────┬───────┘    │
│                │  │         │            │
│                │  │  ┌──────▼───────┐    │
│                │  │  │   Syncer     │    │
│                │  │  │ (async xfer) │    │
│                │  │  └──────┬───────┘    │
│                │  │         │            │
│                │  │  ┌──────▼────────┐   │
│                │  │  │ Remote Store  │   │
│                │  │  │ s3 / memory   │   │
│                │  │  │ (ref counted) │   │
│                │  │  └───────────────┘   │
└────────────────┘  └──────────────────────┘
```

### Key Interfaces

**1. Runtime** (`pkg/controlplane/runtime/`)
- **Single entrypoint for all operations** - both API handlers and internal code
- Updates both persistent store AND in-memory state together
- Thin composition layer delegating to 6 focused sub-services:
  - `adapters/`: Protocol adapter lifecycle management (create, start, stop, delete)
  - `stores/`: Metadata store registry
  - `shares/`: Share registration and configuration; owns per-share `*engine.BlockStore` instances
  - `mounts/`: Unified mount tracking across protocols
  - `lifecycle/`: Server startup/shutdown orchestration
  - `identity/`: Share-level identity mapping
- Key methods:
  - `Serve(ctx)`: Starts all adapters and servers, blocks until shutdown
  - `CreateAdapter(ctx, cfg)`: Saves to store AND starts immediately
  - `DeleteAdapter(ctx, type)`: Stops adapter AND removes from store
  - `AddAdapter(adapter)`: Direct adapter injection (for testing)
  - `GetBlockStoreForHandle(ctx, handle)`: Resolves per-share BlockStore from a file handle via `shares.Service`

**2. Control Plane Store** (`pkg/controlplane/store/`)
- Persistent configuration (users, groups, permissions, adapters)
- Decomposed into 9 sub-interfaces: `UserStore`, `GroupStore`, `ShareStore`, `PermissionStore`, `MetadataStoreConfigStore`, `BlockStoreConfigStore`, `AdapterStore`, `SettingsStore`, `GuestStore`
- Composite `Store` interface embeds all sub-interfaces
- API handlers accept narrowest interface needed
- SQLite (single-node) or PostgreSQL (distributed)

**3. Adapter Interface** (`pkg/adapter/adapter.go`)
- Each protocol implements the `Adapter` interface
- `IdentityMappingAdapter` extends `Adapter` with `auth.IdentityMapper` for protocol-specific identity mapping
- Adapters receive a Runtime reference to access services
- `BaseAdapter` provides shared TCP lifecycle, default `MapError` and `MapIdentity` stubs
- Lifecycle: `SetRuntime() -> Serve() -> Stop()`
- Multiple adapters can share the same runtime
- Thread-safe, supports graceful shutdown

**4. Auth** (`pkg/auth/`)
- Centralized authentication abstractions shared across all protocols
- `AuthProvider` interface: `CanHandle(token)` + `Authenticate(ctx, token)`
- `Authenticator`: Chains multiple providers, tries each in order
- `Identity`: Protocol-neutral authenticated identity (Unix creds, Kerberos, NTLM, anonymous)
- `IdentityMapper` interface: Converts `AuthResult` to protocol-specific identity
- Sub-packages:
  - `kerberos/`: Kerberos `AuthProvider` with keytab management and hot-reload

**5. MetadataService** (`pkg/metadata/`)
- **Central service for all metadata operations**
- Routes operations to the correct store based on share name
- Owns LockManager per share (for SMB/NLM byte-range locking)
- Split into focused files:
  - `file_create.go`, `file_modify.go`, `file_remove.go`, `file_helpers.go`, `file_types.go`: File operations
  - `auth_identity.go`, `auth_permissions.go`: Identity resolution and permission checks
- Protocol handlers should use this instead of stores directly
- `storetest/`: Metadata store conformance test suite (all implementations must pass)

**Recycle bin (trash).** The recycle trap lives inside `MetadataService.RemoveFile`, `RemoveDirectory`, and `Move`, gated by a per-share `TrashPolicy` read through a locked accessor. When the policy enables the bin, an unlink (NFS REMOVE/RMDIR, SMB delete-on-close) or a replace-overwrite (a `Move` whose destination clobbers an existing node) moves the victim into a single shared `#recycle` directory at the share root instead of destroying it, preserving the original path subtree and owner. Block deletion is deferred: recycling returns an empty `PayloadID` so protocol adapters skip the block-deletion step, and a recycled node keeps its content blocks until it is reaped or the bin is emptied. The runtime's `trash.Service` (`pkg/controlplane/runtime/trash/`) owns list/restore/empty and runs a background reaper that enforces the per-share retention-days and max-size policy on an hourly interval (oldest-first eviction). Disabling trash auto-empties the bin.

**6. BlockStore** (`pkg/block/`)
- Per-share block storage orchestrator. Each share gets its own `*engine.BlockStore` instance.
- `engine.BlockStore` composes `local.LocalStore + remote.RemoteStore + engine.Syncer`
- Each share gets an isolated local storage directory; remote stores can be shared across shares (ref counted)
- `shares.Service` owns the lifecycle (create on AddShare, close on RemoveShare)
- Sub-packages:
  - `engine/`: BlockStore orchestrator — composes local + remote stores and owns the unified CAS-keyed `Cache` (read buffering + prefetch), the syncer, and the garbage collector. See `pkg/block/engine/cache.go` for the Cache type.
  - `local/`: Local store interface and implementations (`fs/` filesystem, `memory/` in-memory)
  - `remote/`: Remote store interface and implementations (`s3/` production, `memory/` testing)
  - `storetest/`: Conformance test helpers for new backend implementations

**7. Metadata Store** (`pkg/metadata/store.go`)
- **Simple CRUD interface** for file/directory metadata
- Stores file structure, attributes, permissions
- Implementations:
  - `pkg/metadata/store/memory/`: In-memory (fast, ephemeral, full hard link support)
  - `pkg/metadata/store/badger/`: BadgerDB (persistent, embedded, path-based handles)
  - `pkg/metadata/store/postgres/`: PostgreSQL (persistent, distributed, UUID-based handles)
- File handles are opaque identifiers (implementation-specific format)

## Per-Share Block Store Isolation

Each share in DittoFS gets its own `*engine.BlockStore` instance, providing complete data isolation between shares.

### How It Works

1. **Share Creation**: When a share is added via `dfsctl share create`, the runtime creates a dedicated BlockStore instance with:
   - An isolated local storage directory (under the configured local store path)
   - A reference to the configured remote store (shared across shares via ref counting)

2. **Handle Resolution**: Protocol handlers call `GetBlockStoreForHandle(ctx, handle)` which:
   - Extracts the share name from the file handle
   - Returns the share's dedicated BlockStore instance
   - There is no global BlockStore

3. **Share Removal**: When a share is removed, its BlockStore is closed:
   - Local storage directory is cleaned up
   - Remote store reference count is decremented
   - If ref count reaches zero, the remote store connection is closed

### Isolation Properties

- **Data Isolation**: Each share's local blocks are stored in separate directories
- **Cache Independence**: The unified `Cache` is per-share (eviction in one share does not affect others). Inside a share, the cache is keyed by `ContentHash`, so two files referencing the same chunk via dedup share one cache entry.
- **Remote Sharing**: Multiple shares can reference the same remote store (e.g., same S3 bucket). Chunk bytes are packed into `blocks/<id>` container objects; identical chunks dedup by content hash across every share that targets the same bucket+prefix. For isolation, give shares different buckets or prefixes
- **Lifecycle Independence**: Block stores are created/closed with share lifecycle

## Storage Tiers

DittoFS uses a three-tier storage model for block data:

```
┌─────────────────────────────────────┐
│  Cache (In-Memory, CAS-keyed)       │
│  pkg/block/engine/cache.go     │
│  - Single type, keyed by ContentHash│
│  - LRU eviction                     │
│  - Internal sequential prefetch     │
│    (3-trigger threshold)            │
│  - Cross-file dedup                 │
│  - RAM budget auto-sized per share  │
│    from available system memory     │
│  - Volatile (lost on restart)       │
└──────────────┬──────────────────────┘
               │ cache miss
               ▼
┌─────────────────────────────────────┐
│  Local Journal (write-back cache)   │
│  pkg/block/journal/                 │
│  - Append-only, log-structured      │
│  - Absorbs writes, local-ack        │
│  - Persistent across restarts       │
│  - Per-share isolated directory     │
└──────────────┬──────────────────────┘
               │ cold read (range not cached)
               ▼
┌─────────────────────────────────────┐
│  Remote Store                       │
│  pkg/block/remote/s3/          │
│  - S3 or compatible object store    │
│  - Slowest (network I/O)            │
│  - Durable (survives node loss)     │
│  - Shared across shares (ref count) │
└─────────────────────────────────────┘
```

**Read Path**: `Engine.ReadAt` resolves a file's bytes by `(payloadID,
offset)`. If the range is present in the local journal it is served straight
from a journal segment. On a cold miss the engine resolves the chunk's remote
locator from the FileChunk manifest, issues a ranged read into its enclosing
`blocks/<id>` object, decodes and BLAKE3-verifies the chunk end-to-end
(fail-closed), then hydrates the bytes back into the journal so subsequent
reads are warm. A per-payload sequential tracker drives remote prefetch.

**Write Path**: `Engine.WriteAt` appends the dirty range to the local journal
and acknowledges immediately (write-back) — there is no synchronous chunking,
hashing, or upload on the client path. A background carve pass later packs the
accumulated dirty ranges into remote blocks (see below). How durable the ack is
depends on the configured durability tier (`writeback` / `local-durable` /
`remote`; see the [durability guide](../guide/durability.md)).

**Eviction**:
- Cache: LRU eviction when the RAM budget is reached. No data loss (the journal still holds the bytes). The cache is per-share but cross-file inside a share — the same content hash referenced by two files shares one entry.
- Journal: whole fully-synced segments are evicted approx-LRU under disk pressure. Only ranges already carved to the remote qualify, so eviction never destroys the only copy of dirty bytes. Manual eviction via `dfsctl store block evict`.

## Block Store — Local Journal Tier

The per-share local tier is the **journal** (`pkg/block/journal/`): a single
append-only, log-structured **write-back cache** in front of the remote store.
It replaces the earlier two-tier design (a per-file append-only log plus a
separate rolled-up "log-blob" tier) with one substrate. See the journal
package's own doc comment for the authoritative model; this section covers it
at architecture altitude.

A client write (`WriteAt`) appends a dirty record for `(payloadID, offset)` to
a shared segment file and acknowledges immediately — it never chunks, hashes,
or uploads on the client path, and it never fsyncs (durability is a separate
`Commit`, driven by NFS COMMIT / SMB Flush and the configured durability tier).
Cold-read hydration (`Hydrate`) funnels through the same append primitive, the
only difference being that a hydrated record is born *clean* (already durable
in the remote store, so immediately evictable) while a client write is born
*dirty* (must be carved before it can be evicted). The journal is keyed by
`(payloadID, offset)`, not by content hash.

`*fs.FSStore` (`pkg/block/local/fs/`) is now a **thin adapter** over
`*journal.Store`: it bridges the `string`↔`journal.FileID` keyspace and the
`local.LocalStore` admin surface, and forwards the data-plane calls. The
journal owns its own segment layout, carve, eviction, and local garbage
collection. Only `BackpressureMaxWait` and `ChunkParams` remain load-bearing
knobs; the old rollup/append-log options (`max_log_bytes`, `rollup_workers`,
`stabilization_ms`, `orphan_log_min_age_seconds`) are vestigial — the journal
carves on its own age/size gate.

### Carve: local → remote

A background **carve** pass turns accumulated dirty ranges into remote blocks.
The engine's carve dispatcher (`pkg/block/engine/carve_dispatch.go`) ticks
every `UploadInterval` (default 2s) and asks the journal to carve each file;
the journal applies its own age/size batching gate and serializes carve per
shard internally. Carving a file FastCDC-chunks its dirty ranges (min 1 MiB /
avg 4 MiB / max 16 MiB by default), BLAKE3-hashes each chunk, and — via the
engine-supplied `BlockSink` — dedups against remote-durable chunks, seals each
chunk (compression/encryption), frames the survivors into a packed block
(~16 MiB, `BlockCarveBytes`), uploads the block with one `PutBlock`, and
commits the block record, per-chunk synced markers, and per-file FileChunk
manifest rows in a single metadata transaction (`metadata.DefaultCommitBlock`).
`PutBlock` runs before the commit, so a crash in between leaves an orphan block
object (reclaimed by GC), never an unbacked record; a re-carve targets a fresh
block ID and never double-commits.

Dedup is answered by a durability oracle: a chunk is treated as already remote
iff its hash is present in the per-share `SyncedHashStore`. A share with **no**
remote block store still carves — the local block sink records only the
FileChunk manifest rows (hash + `DataSize`, no remote block key) so clone,
snapshot, and restore can resolve the file's chunks, but nothing is uploaded.

The carve pass fans out across files: a single sequential pass (one file, one
block, one `PutBlock` at a time) leaves the uplink almost idle. Concurrency is
bounded by an **adaptive upload window** (`pkg/block/engine/upload_controller.go`):
a pinned `--parallel-uploads` fixes the window, while the default (adaptive)
mode ramps it between a floor and ceiling to track the goodput knee. Files in
one shard still serialize on the journal's carve lock, so the window overlaps
distinct shards' upload latency.

Explicit `Flush` / `SyncNow` force-carve a file's (or all files') dirty ranges
and serialize against the background dispatcher on the same per-shard lock, so
the two never pack the same range twice. In `ManualSync` mode the background
dispatcher is suppressed and `Flush`/`SyncNow` are the sole carve drivers.

### Reads and integrity

`ReadAt` resolves by `(payloadID, offset)`. A range present in the journal is
served straight from its segment. A range that was written but has since been
evicted returns `cold=true`; the engine then resolves the chunk's remote
locator from the FileChunk manifest, ranged-GETs its enclosing `blocks/<id>`
object, decodes the wire frame, recomputes BLAKE3 over the chunk bytes, and —
on success — hydrates the verified bytes back into the journal so subsequent
reads are warm.

Integrity is fail-closed: every remote fetch is BLAKE3-verified before the
bytes reach the caller, and a mismatch is returned as an error (and counted),
never as data. On the local side, integrity rests on the journal's own
segment-recovery CRC scan at open — there is no per-read local-hash check, so
a cold read that fails remote verification cannot be silently self-healed from
local bytes.

### Eviction and local GC

Under disk pressure the journal evicts whole **fully-synced** segments
approx-LRU; only ranges already carved to the remote qualify, so eviction
never destroys the only copy of dirty bytes. Eviction is health-gated: while
the remote is unhealthy, cold-marking a range would strand unrecoverable
bytes, so eviction is paused. There is no pin/ttl/lru retention knob
(`SetRetentionPolicy` is a no-op on the journal-native store).

The journal's own garbage collection (repack) only relocates live cache bytes
between local segments to reclaim dead space — it **never** touches the remote
store. Reclaiming remote block objects by refcount stays with the engine's
block-GC sweep (see [Garbage Collection](#garbage-collection-mark-sweep)),
whose per-remote serialization is what makes a decrement safe.

### Per-`FSStore` surface

Per the per-share block-store invariants (in `CLAUDE.md`), all journal state —
segments, interval index, carve/eviction machinery, disk budget — lives inside
the per-share `*journal.Store` behind `*FSStore`. No global state is shared
across shares; local storage directories are always isolated.

## Block Lifecycle (three-state)

The block lifecycle has three persisted states held on `FileChunk.State`
indexed by `ContentHash`. There is no parallel state in memory, in fd
pools, or anywhere else: the metadata store is the single source of truth,
and `engine.Syncer` is the sole owner of state transitions.

```
   Pending ──claim batch──▶ Syncing ──PUT success + meta txn──▶ Remote
                              ▲                                    │
                              └──janitor (>claim_timeout)──────────┘
                                                                   │
                                                     (RefCount → 0)│
                                                                   ▼
                                                              GC eligible
```

- **Pending**: `RefCount ≥ 1`; bytes are local; not yet uploaded.
- **Syncing**: a syncer goroutine has claimed the block; the upload is in
  flight.
- **Remote**: PUT of the packed block object returned 200 AND the
  metadata transaction setting `State=Remote` committed (no orphan flag
  without metadata-txn success).

**Restart recovery:** at syncer Start, a one-shot janitor pass
requeues any `Syncing` row whose `last_sync_attempt_at` is older than
`syncer.claim_timeout` (default 10m) back to `Pending`. CAS keys are
content-defined so a duplicate re-upload writes the same bytes to the
same key — idempotent by construction.

**Restart re-drain.** Ranges written to the journal but not yet carved into a
remote block when the process crashed are durable on local disk and are
re-drained automatically on restart: the journal's recovered interval index
re-marks every not-yet-carved record dirty at `Open`, so the carve dispatcher
picks them up on its next tick without any separate reconcile walk or
metadata-side pending set.

**Why a metadata write for every claim?** The Pending → Syncing
transition is the serialization point against duplicate uploads across
syncer instances. The batched-claim cost is one txn per tick, in exchange
for exact restart recovery and a single-query introspection of stuck
blocks (`State=Syncing AND last_sync_attempt_at < now − 1h`).

## Garbage Collection (mark-sweep)

The block-store GC is a fail-closed mark-sweep over the union of every live
`FileChunk.ContentHash` across all shares pointing at the same remote.

### Algorithm

1. **Mark phase.** Stream every `FileChunk`'s `ContentHash` via the
   `MetadataStore.EnumerateFileChunks(ctx, fn)` cursor. The cursor
   is implemented natively per backend (memory, Badger, Postgres) and
   never loads the full set into application memory. Hashes are appended
   to an on-disk live set under `<localStore>/gc-state/<runID>/db/`
   (a Badger temp store). Snapshot time `T` is captured at the start of
   the run. Cross-share aggregation keys on **remote-store identity**
   (`bucket+endpoint+prefix`), not share name, so an object reachable from
   any share that targets the same remote is considered live. Hold
   providers then inject hashes the namespace no longer references but
   that must survive the sweep: snapshot manifests
   (`SnapshotHoldProvider`) and open-but-unlinked files
   (`openHandleHoldProvider`, #1448) — a file unlinked while still held
   open via NFSv4 open stateids or SMB open handles keeps its blocks
   until the last close, restoring POSIX unlink-while-open semantics
   beyond the grace period. NFSv3 is stateless (kernel clients
   silly-rename), so no server-side hold applies there.
2. **Sweep phase.** A single `RemoteStore.Walk` enumerates every CAS
   object cluster-wide; the backend (e.g. S3) paginates internally. For
   each key, the engine keeps the object iff the hash is present in the
   live set OR the object's `LastModified` is newer than
   `T − gc.grace_period` (default 1h). Otherwise the engine issues a DELETE.

### Packed-block reclamation

Because new data lives in packed `blocks/<id>` objects rather than one object
per chunk, the sweep cannot DELETE one remote object per dead hash. Instead
each dead chunk is reclaimed by refcount:

1. A dead `ContentHash` — present in the store's synced-hash index but absent
   from the live set — is resolved to its enclosing block, and
   `DecrLiveChunkCount(blockID, 1)` is applied.
2. When a block's live-chunk count reaches **zero**, the block is fully
   reclaimed: its local blob is evicted, `RemoteBlockStore.DeleteBlock`
   removes the remote object, and the block record is deleted.

A block that still has *any* live chunk is retained by the refcount pass —
packing never deletes a referenced chunk along with its block-mates. Runs are
**serialized per remote** (keyed on remote-store config identity), so the
`LiveChunkCount` that several shares targeting the same remote share is only
ever mutated by one run at a time.

### Compaction of partially-dead blocks

Refcount reclamation alone frees a block only when its *last* live chunk dies,
so a block that keeps a few live chunks but has shed many dead ones pins the
dead bytes forever. Compaction (`engine.CompactBlocks`, #1487) closes that gap.
It runs as an optional final phase of each per-remote sweep, under the same
per-remote lock and immediately **after** the sweep — by which point the sweep
has already cleared the synced marker of every past-grace dead chunk. So a
chunk resident in a block is "still live here" iff its synced locator still
points at that block; a chunk that lost its marker (swept dead) or whose
locator has moved is dropped. This reuses the sweep's own keep/delete decision,
so compaction never reclaims a chunk the sweep would have spared, and needs no
second live-set scan.

- **Candidate selection** is byte-based: the sum of the `WireLength` of every
  live locator pointing at a block, over the block's object `Length`. Below the
  operator's `gc.compaction_live_ratio` (0 disables; a value like `0.5` compacts
  a block once it is more than half dead) the block is a candidate. Computed
  from the block record + locators alone — no per-block download to decide, and
  no extra stored field.
- **Repack** downloads the candidate block once, verifies it against its
  record's whole-block BLAKE3 hash, copies the still-live chunks' wire bodies
  verbatim into a fresh block (the per-chunk encryption already lives in the
  body), `PutBlock`s it, then `DefaultCommitBlock` writes the new record and
  rewrites the moved chunks' locators (last-wins) in one transaction, and
  finally deletes the old block object + record.
- **Crash safety** is identical to the live carver / cas→blocks migration and
  every crash window lands on an existing reconcile class: a crash after
  `PutBlock` before the commit leaves an orphan object (reconcile class 3); a
  crash after the commit before the old block is deleted leaves the old block as
  a leaked record (class 2). A re-run converges — the moved chunks' locators no
  longer point at the old block, so compaction finds nothing to move and just
  deletes the husk.

Because compaction rewrites a live chunk's locator to a new block before
deleting the old one, a reader that resolved the old locator *before* the
rewrite and issues its `GetBlock` *after* the delete sees a miss. This is safe
only because the read path (`dispatchRemoteFetch`, the chokepoint both the
client demand read and background prefetch share) re-resolves the locator once
on `ErrChunkNotFound` and retries against the moved chunk's new block — without
that guard the delete would surface a spurious `EIO` for a perfectly live chunk.

### Fail-closed posture

Mark-phase and sweep-phase failures are treated asymmetrically:

- **Mark errors abort the sweep entirely.** Any uncertainty about the
  live set could lead to deleting referenced data. Sweep workers do not
  start if the mark phase returned any error.
- **Sweep-side per-prefix DELETE errors are captured and continue.** A
  single S3 503 transient should not waste a successful mark phase. The
  run summary reports `error_count` and the first N error samples;
  garbage that survives a transient is reclaimed on the next run.

### gc-state directory layout

```
<localStore>/gc-state/
  20260425T143022Z-abc/
    db/                          (Badger temp store for the live set)
    incomplete.flag              (removed by MarkComplete; cleaned by next run)
  20260425T153122Z-def/
    db/
    (no incomplete.flag — successful run)
  last-run.json                  (most recent GCRunSummary)
```

Each run writes `incomplete.flag` at start; the next run detects stale
directories (by leftover flag) and deletes them before starting fresh.
Mark is idempotent, so resume-on-restart is intentionally not built.

### Triggers and observability

- **Periodic GC is not yet wired.** There is no scheduler; schedule via
  cron until one ships.
- **On-demand** via `dfsctl store block gc <share> [--dry-run]`;
  `--dry-run` skips DELETEs and prints up to `gc.dry_run_sample_size`
  candidate keys (default 1000). The mark-sweep is global across every
  share that targets the same remote, so the `<share>` argument selects
  which remote(s) to scan rather than scoping the live set to one share.
- **Observability** via structured slog INFO at start/end with `run_id`,
  `hashes_marked`, `objects_swept`, `bytes_freed`, `duration_ms`, and
  `error_count`, plus a persisted summary at
  `<localStore>/gc-state/last-run.json`. Inspect with
  `dfsctl store block gc-status <share>`.

GC coordinates with the share-snapshots subsystem through a single
rule: **manifest-on-disk = block held**. Snapshots register a hold
implicitly by writing a `manifest.json` under
`<localStoreDir>/snapshots/<share>/<id>/`. GC's mark phase enumerates
every manifest file at sweep start and unions the referenced hashes
into its retention set, so any block referenced by any snapshot
survives the sweep. The provider that exposes this hold to the GC
layer is `SnapshotHoldProvider`. No hold flag lives in any database
table — the disk is the source of truth.

See [SNAPSHOTS.md](../guide/snapshots.md#10-gc-hold-semantics) for the
operator-facing description of the hold semantics, including the
delete-vs-GC race window.

See `docs/CONFIGURATION.md` for every `gc.*` and `syncer.*` knob, and
`docs/CLI.md` for the `dfsctl store block gc` reference.

## Share Snapshots

Share snapshots are point-in-time, reference-based protection for a
share's content. The subsystem produces three artifacts per snapshot
on local disk and one row in the control-plane database; it does not
copy any block data. See [SNAPSHOTS.md](../guide/snapshots.md) for the
operator-facing runbook; this section describes the architectural
layout and the orchestration flows.

### Subsystem layout

| Location | Role |
|---|---|
| `pkg/snapshot/` | Verify gate, hash-manifest read/write, helper types. |
| `pkg/controlplane/runtime/snapshot.go` | `Runtime.CreateSnapshot`, `WaitForSnapshot`, `RestoreSnapshot`, `GetSnapshot`, `ListSnapshots`, `DeleteSnapshot`. Composition over the metadata store, block store, and snapshot store. |
| `pkg/controlplane/runtime/snapshot_hold.go` | `SnapshotHoldProvider` — per-share delete lock + manifest-on-disk hold surface for GC. |
| `pkg/controlplane/models/snapshot.go` | `Snapshot` GORM model; `SnapshotDir`, `ManifestPath`, `MetadataDumpPath` path helpers. |
| `pkg/controlplane/store/snapshots.go` | `SnapshotStore` CRUD (`GetSnapshot`, `ListSnapshots`, `DeleteSnapshot`). |
| `pkg/controlplane/api/dto/snapshot.go` | Neutral wire DTOs imported by both the REST handler and the apiclient. No GORM types cross the wire. |
| `internal/controlplane/api/handlers/snapshot.go` | Five REST handlers (`Create`, `List`, `Get`, `Delete`, `Restore`), the narrow `SnapshotRuntime` interface (testability seam), and the single `mapSnapshotError` sentinel-to-HTTP table. |
| `pkg/apiclient/snapshots.go` | Typed Go client (6 methods) re-exporting the wire DTOs as type aliases of `dto.Snapshot`. |
| `cmd/dfsctl/commands/share/snapshot/` | Five cobra leaf commands matching the REST surface (`create`, `list`, `show`, `delete`, `restore`). |

### On-disk artifacts

Every snapshot owns a directory under the share's local store:

```
<localStoreDir>/snapshots/<share>/<snap-id>/
  ├─ metadata.dump          ← engine-native metadata serialization
  └─ manifest.json          ← BLAKE3 hashes of every CAS block the share references
```

`SnapshotDir(localStoreDir)`, `ManifestPath(localStoreDir)`, and
`MetadataDumpPath(localStoreDir)` on the `Snapshot` model compute the
canonical paths. Atomic write is via `temp + rename` so a partial
manifest never surfaces to the GC enumeration step. The manifest
file's existence is the GC hold; there is no separate hold record.

### Create orchestration

```
CreateSnapshot ─→ persist Snapshot row (state=creating)
              ─→ DrainAllUploads (skipped if NoVerify)
              ─→ Dump metadata to metadata.dump
              ─→ Build hash manifest from CAS
              ─→ VerifyRemoteDurability (skipped if NoVerify, concurrency = 16)
              ─→ Update row state=ready (or failed) + remote_durable flag
```

`Runtime.CreateSnapshot` returns the new snapshot ID immediately and
runs the orchestration in a background goroutine. The REST handler
returns `202 Accepted` with a `Location` header pointing at the
record; callers poll `GET /snapshots/{id}` until `state != "creating"`.
The CLI's `WaitForSnapshot` does that polling on the operator's
behalf.

`NoVerify=true` (CLI `--no-verify`) skips both the upload drain and
the HEAD-probe phase. The snapshot still completes with
`remote_durable=false`. Restore of a non-durable snapshot then
requires the explicit `AllowNonDurable` flag (CLI `--force`).

### Restore orchestration

```
RestoreSnapshot ─→ Pre-flight: refuse if share enabled
                ─→ Verify source snapshot's remote durability
                   (skipped if AllowNonDurable)
                ─→ Pre-restore safety snapshot (ID returned to caller)
                ─→ Close metadata store
                ─→ Reset (via Resetable interface)
                ─→ Restore from metadata.dump
                ─→ HashSetFromMetadataStore walk
                ─→ Post-restore block verify
```

`Runtime.RestoreSnapshot` returns `(safetySnapshotID, err)`. The
safety snap ID is set as soon as step 3 succeeds, even if a later
step fails — callers (REST + CLI) surface the ID to the operator so
the rollback path is always available without a separate
`ListSnapshots` filter. On precheck / pre-verify failure (before
step 3) the safety ID is the empty string.

### Per-share delete lock

`SnapshotHoldProvider.AcquireDeleteLock(share)` returns a release
function around a per-share `*sync.RWMutex`. The same mutex
serializes `CreateSnapshot`, `RestoreSnapshot`, and
`DeleteSnapshot` on the same share so that:

- Two concurrent `delete` calls on different snapshots of the same
  share cannot race the per-snapshot directory wipe against each
  other.
- A `delete` cannot race a `create` whose manifest write would
  appear in the snapshots directory mid-sweep.
- A `restore` cannot race a `delete` of the safety snap it is about
  to create.

`Runtime.DeleteSnapshot` is the canonical entry point — handlers
never reach into `r.store.DeleteSnapshot` directly. The wrapper owns
the lock acquisition, the database row delete, the on-disk
directory wipe, and the lock release.

### HTTP surface

Five REST endpoints under `/api/v1/shares/{name}/snapshots` (admin
only, inherits the existing `RequireAdmin` middleware):

| Method | Path | Result |
|---|---|---|
| `POST` | `/` | 202 Accepted + `Location` header |
| `GET` | `/` | 200 OK + JSON array (empty: `[]`, not `null`) |
| `GET` | `/{id}` | 200 OK + full record |
| `DELETE` | `/{id}` | 204 No Content |
| `POST` | `/{id}/restore` | 200 OK + `{snapshot_id, safety_snapshot_id, share}` |

The single `mapSnapshotError` helper handles the 14 typed sentinels
that can cross the boundary (12 snapshot sentinels + share-not-found
+ nil-guard). The mapping table lives in the handler file as the
sole source of truth; future sentinels add a single case.

The Restore handler wraps `r.Context()` with
`context.WithTimeout(ctx, cfg.Snapshot.restore_http_timeout)`
(default 30 minutes) to bound runaway restores. The apiclient
mirrors the timeout on the client's `http.Client` for the restore
call only (`WithRestoreTimeout`).

For the full operator runbook see
[SNAPSHOTS.md](../guide/snapshots.md).

## Block Reads (verified)

A read resolves by `(payloadID, offset)` — no remote trial-and-error — and
takes one of two paths:

1. **Local journal hit.** If the range is present in the local journal, its
   bytes are served straight from the journal segment. This is the steady-state
   path for recently-written and recently-read data.
2. **Remote packed block.** On a cold miss (the range was written but has been
   evicted), the engine resolves the chunk's `ChunkLocator`
   (`BlockID` + `[WireOffset, WireOffset+WireLength)`) from the FileChunk
   manifest, issues a ranged GET against the packed remote object
   `blocks/<BlockID>`, decodes the wire frame, and recomputes BLAKE3 over the
   chunk bytes. A verified chunk is hydrated back into the journal so subsequent
   reads take path 1.

A synced chunk whose locator is empty (standalone) or missing is
**post-migration drift** — the startup migration rewrites every standalone
locator to a block locator before the share serves — so such a locator is
refused fail-closed rather than read.

**Fail-closed integrity.** Every remote fetch is BLAKE3-verified before the
bytes reach the caller. A mismatch is never surfaced as data: the read returns
an error and increments a corruption metric. Local integrity rests on the
journal's segment-recovery CRC scan at open; there is no per-read local-hash
check, so a cold read that fails remote verification is not silently
self-healed from local bytes — it fails closed and the corruption is recorded.

The pre-v0.16 non-CAS layout (`{payloadID}/block-{N}`) is no longer read at
runtime. A store directory still on that layout is refused on open, directing
the operator to migrate with dittofs ≤ v0.21. See
[Migration & Block-Layout Routing](#migration--block-layout-routing).

## Adapter Pattern

DittoFS uses the Adapter pattern to provide clean protocol abstractions:

```go
// ProtocolAdapter interface (defined in runtime package to avoid import cycles)
type ProtocolAdapter interface {
    Serve(ctx context.Context) error
    Stop(ctx context.Context) error
    Protocol() string
    Port() int
}

// RuntimeSetter - adapters that need runtime access implement this
type RuntimeSetter interface {
    SetRuntime(rt *Runtime)
}

// Example: NFS Adapter accesses per-share block stores via runtime
type NFSAdapter struct {
    config  NFSConfig
    runtime *runtime.Runtime
}

func (a *NFSAdapter) handleRead(ctx context.Context, req *ReadRequest) {
    // Resolve per-share block store from file handle
    blockStore, err := a.runtime.GetBlockStoreForHandle(ctx, handle)
    // Read data via the block store with a caller-snapshot []BlockRef.
    // The engine binary-searches blocks for the requested range; sparse
    // holes outside any BlockRef are zero-filled.
    n, err := blockStore.ReadAt(ctx, payloadID, attr.Blocks, dest, offset)
    // ...
}

// Multiple adapters can run concurrently, sharing the same runtime
rt := runtime.New(cpStore)
rt.SetAdapterFactory(createAdapterFactory())
rt.Serve(ctx)  // Loads adapters from store and starts them
```

### Shared adapter helpers (internal/adapter/common)

NFSv3, NFSv4, and SMB v2/3 handlers share a single package of helpers at
`internal/adapter/common/` so the three adapters do not each carry a
private copy of the same logic. The package exposes:

- **Block-store resolution**: `common.ResolveForRead` / `common.ResolveForWrite`
  wrap `Runtime.GetBlockStoreForHandle` via a narrow `BlockStoreRegistry`
  interface (satisfied implicitly by `*runtime.Runtime`). All three
  protocols' READ/WRITE/COMMIT paths route through these two calls.
- **Pooled read buffer**: `common.ReadFromBlockStore` returns a
  `BlockReadResult` whose `Release()` is handed to the response encoder,
  which invokes it after the wire write completes. NFSv3, NFSv4, and SMB
  regular-file READ all adopt the pool; pipe/symlink READ paths stay on
  heap allocations by design (documented in SMB.md).
- **`[]BlockRef` seam**: `common.ReadFromBlockStore`,
  `common.WriteToBlockStore`, and `common.CommitBlockStore` are the single
  edit points that feed resolved `[]BlockRef` into the engine. Handler code
  stays untouched; changes to the block-ref threading stay confined to
  `common/`.
- **Metadata error translation**: a struct-per-code table (`errorMap` in
  `common/errmap.go`) with NFS3/NFS4/SMB columns; `common.MapToNFS3`,
  `common.MapToNFS4`, and `common.MapToSMB` are thin accessors. Lock-
  operation context uses the parallel `lockErrorMap` (`common/lock_errmap.go`)
  which overrides a handful of codes (e.g., `ErrLocked` →
  `STATUS_LOCK_NOT_GRANTED` in lock context vs. `STATUS_FILE_LOCK_CONFLICT`
  in general I/O context). Adding a new `metadata.ErrorCode` is one edit
  across all three protocols — the struct literal requires every column
  to be populated, so you cannot ship a code that is missing an NFS or
  SMB mapping.

See CONTRIBUTING.md "Adding a new metadata.ErrorCode" for the recipe and
NFS.md / SMB.md "Error mapping" for protocol-specific notes.

## Control Plane Pattern

The Control Plane is the central management component enabling flexible, multi-share configurations.

### How It Works

1. **Named Store Creation**: Stores are created with unique names (e.g., "fast-memory", "s3-archive")
2. **Share-to-Store Mapping**: Each share references metadata and block stores by name
3. **Handle Identity**: File handles encode both the share ID and file-specific data
4. **Store Resolution**: When handling operations, the runtime decodes the handle to identify the share, then routes to the correct stores

### Configuration Example

Stores, shares, and adapters are managed at runtime via `dfsctl` (persisted in the control plane database):

```bash
# Create named stores (created once, shared across shares)
./dfsctl store metadata add --name fast-meta --type memory
./dfsctl store metadata add --name persistent-meta --type badger \
  --config '{"path":"/data/metadata"}'

# Create block stores (local per-share, remote shared across shares)
./dfsctl store block add --kind local --name local-cache --type fs \
  --config '{"path":"/data/cache"}'
./dfsctl store block add --kind remote --name s3-remote --type s3 \
  --config '{"region":"us-east-1","bucket":"my-bucket"}'

# Create shares referencing stores by name (each gets its own BlockStore)
./dfsctl share create --name /temp --metadata fast-meta --local local-cache
./dfsctl share create --name /archive --metadata persistent-meta \
  --local local-cache --remote s3-remote
```

### Benefits

- **Per-share isolation**: Each share gets its own BlockStore with isolated local storage directory
- **Resource Efficiency**: Remote stores are shared (ref counted) when multiple shares reference the same config
- **Flexible Topologies**: Mix local-only and remote-backed storage per-share
- **Future Multi-Tenancy**: Foundation for per-tenant store isolation

## Service Layer

The service layer provides business logic and coordination between stores.

### MetadataService

Handles all metadata operations with share-based routing:

```go
// MetadataService - central service for metadata operations
type MetadataService struct {
    stores       map[string]MetadataStore  // shareName -> store
    lockManagers map[string]*LockManager   // shareName -> lock manager
}

// Usage by protocol handlers
metaSvc := metadata.New()
metaSvc.RegisterStoreForShare("/export", memoryStore)
metaSvc.RegisterStoreForShare("/archive", badgerStore)

// High-level operations (with business logic)
file, err := metaSvc.CreateFile(authCtx, parentHandle, "test.txt", fileAttr)
entries, err := metaSvc.ReadDir(ctx, dirHandle)

// Byte-range locking (SMB/NLM)
lock, err := metaSvc.AcquireLock(ctx, shareName, handle, offset, length, exclusive)
```

### Write Coordination Pattern

WRITE operations require coordination between metadata and block stores:

```go
// 1. Update metadata (validates permissions, updates size/timestamps);
//    capture the caller-snapshot []BlockRef for the engine.
attr, preSize, preMtime, preCtime, err := metadataStore.WriteFile(handle, newSize, authCtx)
currentBlocks := attr.Blocks  // []blockstore.BlockRef sorted by Offset

// 2. Resolve per-share block store from file handle
blockStore, err := rt.GetBlockStoreForHandle(ctx, handle)

// 3. Write actual data via per-share block store; engine FastCDC-rechunks
//    the affected range and returns the new []BlockRef.
newBlocks, err := blockStore.WriteAt(ctx, string(attr.PayloadID), currentBlocks, data, offset)

// 4. Persist newBlocks in the same metadata txn that updates Size/Mtime.
//    The engine never opens the metadata txn itself.
err = metadataStore.SetFileChunks(handle, newBlocks, authCtx)

// 5. Post-txn surgical cache invalidation: drop only the hashes that
//    disappeared, preserving warm dedup entries.
removed := diffRemovedHashes(currentBlocks, newBlocks)
blockStore.Cache().InvalidateFile(string(attr.PayloadID), removed)

// 6. Return updated attributes to client for cache consistency
```

## Built-In and Custom Backends

### Using Built-In Backends

No custom code required - configure via CLI:

```bash
# Create stores
./dfsctl store metadata add --name default-meta --type memory  # or badger, postgres
./dfsctl store block add --kind local --name default-local --type fs \
  --config '{"path":"/data/blocks"}'

# Create share referencing stores
./dfsctl share create --name /export --metadata default-meta --local default-local
```

### Implementing Custom Store Backends

See [docs/IMPLEMENTING_STORES.md](implementing-stores.md) for detailed implementation guides for:
- **Local Store**: Implement `pkg/block/local.LocalStore` interface
- **Remote Store**: Implement `pkg/block/remote.RemoteStore` interface
- **Metadata Store**: Implement `pkg/metadata/Store` interface

## Directory Structure

```
dittofs/
├── cmd/
│   ├── dfs/                      # Server CLI binary
│   │   ├── main.go               # Entry point
│   │   └── commands/             # Cobra commands (start, stop, config, logs)
│   └── dfsctl/                   # Client CLI binary
│       ├── main.go               # Entry point
│       ├── cmdutil/              # Shared utilities (auth, output, flags)
│       └── commands/             # Cobra commands (user, group, share, store, adapter)
│
├── pkg/                          # Public API (stable interfaces)
│   ├── adapter/                  # Protocol adapter interface
│   │   ├── adapter.go            # Adapter + IdentityMappingAdapter interfaces
│   │   ├── auth.go               # Adapter-level Authenticator interface
│   │   ├── base.go               # BaseAdapter shared TCP lifecycle
│   │   ├── errors.go             # ProtocolError interface
│   │   ├── nfs/                  # NFS adapter implementation
│   │   └── smb/                  # SMB adapter implementation
│   │
│   ├── auth/                     # Centralized authentication abstractions
│   │   ├── auth.go               # AuthProvider, Authenticator, AuthResult
│   │   ├── identity.go           # Identity model, IdentityMapper interface
│   │   └── kerberos/             # Kerberos AuthProvider
│   │       ├── provider.go       # Provider (implements AuthProvider)
│   │       ├── keytab.go         # Keytab hot-reload manager
│   │       └── doc.go            # Package doc
│   │
│   ├── metadata/                 # Metadata layer
│   │   ├── service.go            # MetadataService (business logic, routing)
│   │   ├── store.go              # MetadataStore interface (CRUD)
│   │   ├── file_create.go        # File/directory creation operations
│   │   ├── file_modify.go        # File modification operations
│   │   ├── file_remove.go        # File removal operations
│   │   ├── file_helpers.go       # Shared file operation helpers
│   │   ├── file_types.go         # File-related type definitions
│   │   ├── auth_identity.go      # Identity resolution
│   │   ├── auth_permissions.go   # Permission checking
│   │   ├── cookies.go            # CookieManager (NFS/SMB pagination)
│   │   ├── types.go              # FileAttr, DirEntry, etc.
│   │   ├── errors.go             # Metadata-specific errors
│   │   ├── locking.go            # LockManager for byte-range locks
│   │   ├── storetest/            # Conformance test suite for store implementations
│   │   └── store/                # Store implementations
│   │       ├── memory/           # In-memory (ephemeral)
│   │       ├── badger/           # BadgerDB (persistent)
│   │       └── postgres/         # PostgreSQL (distributed)
│   │
│   ├── blockstore/               # Per-share block storage
│   │   ├── doc.go                # Package documentation
│   │   ├── store.go              # FileChunkStore interface
│   │   ├── types.go              # FileChunk, BlockState types
│   │   ├── errors.go             # BlockStore error types
│   │   ├── chunker/              # FastCDC content-defined chunker
│   │   │                         # min=1 MiB / avg=4 MiB / max=16 MiB, lvl 2;
│   │   │                         # BLAKE3 hashing; consumed by the carve pass
│   │   ├── engine/               # BlockStore orchestrator + read cache + syncer + GC
│   │   ├── journal/              # Local write-back cache (append-only segments)
│   │   ├── local/                # Local store interface
│   │   │   ├── fs/               # Thin adapter over pkg/block/journal
│   │   │   └── memory/           # In-memory local store (testing)
│   │   └── remote/               # Remote store interface
│   │       ├── s3/               # S3-backed remote store
│   │       └── memory/           # In-memory remote store (testing)
│   │
│   ├── controlplane/             # Control plane (config + runtime)
│   │   ├── store/                # GORM-based persistent store
│   │   │   ├── interface.go      # 9 sub-interfaces + composite Store
│   │   │   ├── gorm.go           # GORMStore implementation
│   │   │   ├── helpers.go        # Generic GORM helpers
│   │   │   └── ...               # Per-entity implementations
│   │   ├── runtime/              # Ephemeral runtime state
│   │   │   ├── runtime.go        # Composition layer (~500 lines)
│   │   │   ├── adapters/         # Adapter lifecycle sub-service
│   │   │   ├── stores/           # Metadata store registry sub-service
│   │   │   ├── shares/           # Share management sub-service
│   │   │   ├── mounts/           # Unified mount tracking sub-service
│   │   │   ├── lifecycle/        # Serve/shutdown orchestration sub-service
│   │   │   └── identity/         # Identity mapping sub-service
│   │   ├── api/                  # REST API server
│   │   │   ├── server.go         # HTTP server with JWT
│   │   │   └── router.go         # Route definitions
│   │   └── models/               # Domain models (User, Group, Share)
│   │
│   ├── apiclient/                # REST API client library
│   │   ├── client.go             # HTTP client with token auth
│   │   ├── helpers.go            # Generic API client helpers
│   │   └── ...                   # Resource-specific methods
│   │
│   └── config/                   # Configuration parsing
│       ├── config.go             # Main config struct
│       ├── stores.go             # Store creation
│       └── runtime.go            # Runtime initialization
│
├── internal/                     # Private implementation details
│   ├── adapter/common/           # Shared NFS/SMB adapter helpers: block-store
│   │   │                         # resolution (ResolveForRead/Write), pooled
│   │   │                         # ReadFromBlockStore + WriteToBlockStore +
│   │   │                         # CommitBlockStore ([]BlockRef seam), and the
│   │   │                         # consolidated metadata.ErrorCode ->
│   │   │                         # NFS3/NFS4/SMB mapping tables.
│   │   ├── resolve.go            # BlockStoreRegistry narrow interface +
│   │   │                         # ResolveForRead/Write
│   │   ├── read_payload.go       # Pooled BlockReadResult + ReadFromBlockStore
│   │   ├── write_payload.go      # WriteToBlockStore + CommitBlockStore seams
│   │   ├── errmap.go             # Struct-per-code table (NFS3/NFS4/SMB columns)
│   │   ├── content_errmap.go     # Block-store content error table
│   │   └── lock_errmap.go        # Lock-context error table
│   ├── adapter/nfs/              # NFS protocol implementation
│   │   ├── dispatch.go           # RPC procedure routing
│   │   ├── rpc/                  # RPC layer (call/reply handling)
│   │   │   └── gss/              # RPCSEC_GSS framework
│   │   ├── core/                 # Generic XDR codec
│   │   ├── types/                # NFS constants and types
│   │   ├── mount/handlers/       # Mount protocol procedures
│   │   ├── v3/handlers/          # NFSv3 procedures (READ, WRITE, etc.)
│   │   └── v4/handlers/          # NFSv4.0 and v4.1 procedures
│   ├── adapter/smb/              # SMB protocol implementation
│   │   ├── auth/                 # NTLM/SPNEGO authentication
│   │   ├── framing.go            # NetBIOS framing
│   │   ├── dispatch.go           # Command dispatch
│   │   └── v2/handlers/          # SMB2 command handlers
│   ├── controlplane/api/         # API implementation
│   │   ├── handlers/             # HTTP handlers with centralized error mapping
│   │   └── middleware/           # Auth middleware
│   └── logger/                   # Logging utilities
│
├── docs/                         # Documentation
│   ├── ARCHITECTURE.md           # This file
│   ├── CONFIGURATION.md          # Configuration guide
│   └── ...
│
└── test/                         # Test suites
    ├── integration/              # Integration tests (S3, BadgerDB)
    └── e2e/                      # End-to-end tests (real NFS mounts)
```

## Horizontal Scaling with PostgreSQL

The PostgreSQL metadata store enables horizontal scaling for high-availability and high-throughput deployments:

### Architecture

```
┌─────────────┐  ┌─────────────┐  ┌─────────────┐
│  DittoFS #1 │  │  DittoFS #2 │  │  DittoFS #3 │
│  (Pod 1)    │  │  (Pod 2)    │  │  (Pod 3)    │
└──────┬──────┘  └──────┬──────┘  └──────┬──────┘
       │                │                │
       └────────────────┼────────────────┘
                        │
                   ┌────▼─────┐
                   │PostgreSQL│
                   │ Cluster  │
                   └──────────┘
```

### Key Features

1. **Multiple DittoFS Instances**: Run multiple instances sharing one PostgreSQL database
2. **Load Balancing**: Use Kubernetes services or external load balancers to distribute requests
3. **No Session Affinity Required**: Any instance can serve any request (stateless design)
4. **Independent Connection Pools**: Each instance maintains its own connection pool (10-15 conns typical)
5. **Statistics Caching**: 5-second TTL cache reduces database load
6. **ACID Transactions**: Ensures consistency across concurrent operations

### Deployment Example (Kubernetes)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: dfs
spec:
  replicas: 3  # Multiple instances for HA
  selector:
    matchLabels:
      app: dfs
  template:
    metadata:
      labels:
        app: dfs
    spec:
      containers:
      - name: dfs
        image: dfs:latest
        ports:
        - containerPort: 12049
          name: nfs
        env:
        - name: DITTOFS_METADATA_POSTGRES_HOST
          value: postgres-service
        - name: DITTOFS_METADATA_POSTGRES_PASSWORD
          valueFrom:
            secretKeyRef:
              name: postgres-secret
              key: password
        resources:
          requests:
            memory: "256Mi"
            cpu: "250m"
          limits:
            memory: "512Mi"
            cpu: "500m"
---
apiVersion: v1
kind: Service
metadata:
  name: dfs-nfs
spec:
  selector:
    app: dfs
  ports:
  - port: 2049
    targetPort: 12049
    protocol: TCP
  type: LoadBalancer
```

### Connection Pool Sizing

Connection pool sizing depends on your workload:

- **Light workload** (< 10 concurrent clients): `max_conns: 10`
- **Medium workload** (10-50 concurrent clients): `max_conns: 15`
- **Heavy workload** (50+ concurrent clients): `max_conns: 20-25`

**Formula**: `max_conns ~ 2 x expected_concurrent_operations`

**PostgreSQL Limits**: Ensure PostgreSQL `max_connections` > `(DittoFS instances x max_conns)`

Example: 3 DittoFS instances x 15 conns = 45 total connections needed from PostgreSQL

### Performance Considerations

- **Network Latency**: PostgreSQL adds ~1-2ms latency per metadata operation
- **Statistics Caching**: Reduces expensive queries (disk usage, file counts)
- **Query Optimization**: All queries use indexed fields for fast lookups
- **Transaction Overhead**: Short-lived transactions minimize lock contention

### Best Practices

1. **Use Connection Pooling**: Keep `max_conns` reasonable (10-20 per instance)
2. **Enable TLS**: Use `sslmode: require` or higher in production
3. **Monitor Connections**: Watch PostgreSQL connection count and utilization
4. **Scale Horizontally**: Add DittoFS replicas, not connection pool size
5. **Separate Read Replicas**: For read-heavy workloads, consider PostgreSQL read replicas

## Durable Handle State Flow

SMB3 durable handles allow open file state to survive client disconnects and (with persistent backends) server restarts. The lifecycle is:

```
OPEN -[disconnect]-> ORPHANED -[scavenger timeout]-> EXPIRED -[cleanup]-> CLOSED
                         |                                        |
                         +-[reconnect]--> RESTORED --> OPEN       |
                         |                                        |
                         +-[conflict/app-instance]--> FORCE_EXPIRED --> CLOSED
```

**Grant**: CREATE with DHnQ/DH2Q context triggers durability check. If the oplock level and share mode allow it, the server grants a durable handle with a configurable timeout (default 60s).

**Disconnect**: On connection loss, `closeFilesWithFilter` checks `IsDurable`. Durable files are persisted to `DurableHandleStore` (locks and leases preserved) rather than closed.

**Scavenger**: A background goroutine (`DurableHandleScavenger`) runs at 10-second intervals. For each expired handle it performs cleanup: releases byte-range locks, flushes block store caches, then deletes the handle from the store. On server restart, the scavenger adjusts remaining timeouts to account for downtime.

**Reconnect**: A new session sends CREATE with DHnC/DH2C. The server validates the durable-handle context against stored state (share name, path, username, session key hash, FileID, DesiredAccess, ShareAccess, expiry, and file existence) and restores the `OpenFile` without data loss.

**Conflict**: When a new open targets a file with an orphaned durable handle, the scavenger force-expires the orphaned handle to allow the new open to proceed. Cleanup includes releasing byte-range locks and flushing block store caches.

**App Instance ID**: For Hyper-V failover, a CREATE with a matching `AppInstanceId` triggers force-close of the old handle, allowing the new VM instance to take over.

**Admin API**: `GET /api/v1/durable-handles` lists all active handles with remaining timeout. `DELETE /api/v1/durable-handles/{id}` force-closes a specific handle.

## Engine API + BlockRef + Cache

The read path is structured so the engine never imports `pkg/metadata` on
hot paths; it consumes a caller-supplied `[]BlockRef` snapshot as the
authoritative content list for every file.

### BlockRef — the content unit

`BlockRef` is the 3-tuple `(Hash ContentHash, Offset uint64, Size uint32)`
defined in `pkg/block/types.go`. `FileAttr.Blocks []BlockRef` (in
`pkg/metadata/file_types.go`) is the authoritative, offset-sorted list of
every chunk that composes a file. It is populated on every sync
finalization; the engine binary-searches it via `findBlocksForRange`
(`pkg/block/engine/range.go`).

Storage encodings differ per backend:

- **Postgres** uses a separate `file_block_refs` table with PK
  `(file_id, offset) INCLUDE (size, hash)`, FK `ON DELETE CASCADE`, hash
  column `BYTEA`. Random 4 KiB writes touch 1–2 rows instead of rewriting a
  ~1.5 MB TOAST blob.
- **Badger** and **Memory** inline-encode `Blocks []BlockRef` inside the
  existing `FileAttr` blob (gob for Badger, typed structs for Memory).

### Engine API

```go
// pkg/block/engine/engine.go
ReadAt(ctx, payloadID, blocks []BlockRef, dest []byte, offset uint64) (int, error)
WriteAt(ctx, payloadID, currentBlocks []BlockRef, data []byte, offset uint64) ([]BlockRef, error)
Truncate(ctx, payloadID, currentBlocks []BlockRef, newSize uint64) ([]BlockRef, error)
Delete(ctx, payloadID, blocks []BlockRef) error
CopyPayload(ctx, srcPayloadID, srcBlocks []BlockRef, dstPayloadID) ([]BlockRef, error)
```

Range-coverage semantics: `findBlocksForRange(blocks, offset, size)`
returns `[start, end)` of the BlockRef slice that overlaps the requested
range using binary search on the offset-sorted slice; sparse holes
inside `FileAttr.Size` are zero-filled — `no BlockRef for this range` is
documented behavior, not a bug. Past `FileAttr.Size` returns short-read or
EOF.

`CopyPayload` is **O(1)** — a single metadata transaction increments
`FileChunk.RefCount` for every distinct hash in `srcBlocks` and inserts
the dst rows. No data copy. This is the file-level dedup primitive the
ObjectID layer (below) builds on.

`MetadataCoordinator` (`pkg/block/engine/coordinator.go`) is the
narrow interface the engine uses to mutate refcounts and persist
`FileAttr.Blocks`. The engine never opens a metadata txn itself — a
strict-grep build gate enforces zero `pkg/metadata` imports under
`pkg/block/engine/*.go` production files except a single justified
exception in `gc.go`.

### Cache

The `Cache` type (`pkg/block/engine/cache.go`) is keyed solely by
`ContentHash`. It combines read buffering and prefetch into a single
per-share type. The cache is **in-memory (RAM-only), CAS-keyed, and
volatile** — there is no cache-budget config knob. Its byte budget
(`maxBytes`, passed to `NewCache`) is **auto-deduced from available system
memory at startup** (`AvailableMemory / 8`, clamped to a floor; see
`pkg/block/defaults.go`), wired into the engine as `ReadBufferBytes`.
Two files reading the same chunk hit the same entry (cross-file dedup).

```go
// pkg/block/engine/cache.go (hint API)
OnRead(payloadID PayloadID, hashes []ContentHash, fileSize uint64)
InvalidateFile(payloadID PayloadID, removedHashes []ContentHash)  // surgical
```

Sequential prefetch triggers after 3 consecutive sequential reads (to
suppress speculative prefetch on accidental two-block runs in random-IO
workloads). Bounded concurrency: 4 worker goroutines per cache by default.
LRU eviction.

Cache misses load through `local.Get` — a single content-addressed local
read that returns a freshly allocated buffer, which the Cache copies into its
LRU slot. There is no `mmap`/page-cache fast path on any platform (the cache
is RAM-only); the allocation simply moves earlier in the pipeline than the
former mmap-then-copy design.

`InvalidateFile` is **surgical**: the caller passes only the hashes that
disappeared from the file, so other files still referencing those hashes
via dedup keep them warm. Invalidation happens **post-txn** — the caller
commits the new `[]BlockRef` first, then drops cache entries.

### Adapter call sites unchanged

All NFS v3/v4 + SMB v2 protocol handlers stay untouched. The
`internal/adapter/common/{ResolveForRead, ResolveForWrite,
WriteToBlockStore, ReadFromBlockStore}` helpers absorb the `[]BlockRef`
threading, so changes to the read/write path stay confined to the helpers.

### Operator surfaces

- `dfsctl blockstore audit-refcounts <share>` runs the refcount
  reconciliation audit (`∑ FileChunk.RefCount == ∑ len(FileAttr.Blocks)`),
  emits aggregate counts as structured slog INFO, and persists the
  last-run summary at `<localStore>/audit-state/last-inv02.json`. See
  `docs/CLI.md` for the full reference and `docs/FAQ.md` for operator
  guidance.
- The cache has no operator-facing config knobs: its RAM budget is
  auto-deduced from available system memory at startup (see
  `pkg/block/defaults.go`), the sequential-prefetch trigger (3 consecutive
  reads) is fixed, and the prefetch worker count defaults to 4 in code.

## File-Level Dedup: ObjectID + Merkle Root

File-level dedup layers on top of the chunk-level CAS path. Each
`FileAttr` carries an `ObjectID` — a BLAKE3
Merkle root computed over the file's `BlockRef.Hash` values sorted by
`Offset`, prefixed by the domain-separation tag
`dittofs:objectid:v1\x00`:

    ObjectID = BLAKE3("dittofs:objectid:v1\x00" || h0 || h1 || ... || hN-1)

Implemented in `blockstore.ComputeObjectID`
(`pkg/block/objectid.go`). Stable across rename and engine restart
by construction (BLAKE3 + FastCDC are both deterministic; the prefix
protects the output space from per-chunk hash collisions and reserves
room for future input-shape changes via `v2`/`v3`).

### Lifecycle

- **Cleared (zeroed)** on first dirty write that mutates `FileAttr.Blocks`,
  in the same metadata transaction.
- **Recomputed and persisted** at the post-Flush coordinator hook
  (`Syncer.persistFileChunksAfterFlush` → `MetadataCoordinator.PersistFileChunks`),
  in the same metadata transaction that updates `FileAttr.Blocks`/`Size`/`Mtime`.
- **Persisted ONLY on full quiesce** — every block in `Remote` state.
  Partial flushes leave `ObjectID` at zero.

A non-zero `ObjectID` always reflects a fully-`Remote` consistent state.
The dedup short-circuit trusts this without checking per-block states.
Empty files dedup to one canonical constant
`BLAKE3("dittofs:objectid:v1\x00")`; files written before ObjectID existed
keep the all-zero sentinel until the migration tool backfills them.

### File-level dedup short-circuit

When a file's BlockRef list is fully `Pending` (newly chunked, nothing
uploaded yet) and the file has no prior ObjectID, the syncer:

1. Computes the provisional ObjectID over the chunker output.
2. Calls `MetadataStore.FindByObjectID(ctx, objectID)`.
3. **On hit:** increments RefCount on every distinct hash in the
   target's BlockRef list, replaces the file's BlockRef list with the
   target's (deep copy), persists the ObjectID, decrements RefCount on
   any speculative-only hashes, invalidates orphaned cache entries,
   and truncates the per-file append log. **Zero S3 PUTs.**
4. **On miss:** continues per-block GetByHash + PUT path; ObjectID is
   finalized at the post-Flush coordinator hook.

Trigger condition: `len(Blocks) > 0 AND every block.State == Pending AND
file.ObjectID == zero`. This captures fresh-file-create (VM image clone —
the primary target) and full-overwrite (`cp -f`, `dd`-overwrite,
restore-from-backup). It intentionally excludes the running-VM hot path:
incremental writes already get chunk-level dedup via `GetByHash` and would
not benefit from file-level fingerprinting that requires a quiesce.

### Production call chain

The end-to-end wiring. Reads bottom-up; arrows show synchronous dispatch:

```
Production call chain (per-write, on quiesce):

  protocol handler (NFSv3 COMMIT, NFSv4 COMMIT, SMB CLOSE)
    → internal/adapter/common.CommitBlockStore
    → engine.BlockStore.Flush
    → engine.Syncer.Flush
        ├─[file-level dedup short-circuit]
        │   ├─ snapshotPendingBlockRefs(payloadID)         // ListFileChunks projection
        │   ├─ coordinator.GetFileObjectID(payloadID)      // trigger-condition check
        │   ├─ TrySpeculativeFileLevelDedup
        │   │   ├─ ComputeObjectID(specBlocks)
        │   │   ├─ coordinator.FindByObjectID
        │   │   └─ applyFileLevelDedupHit (one metadata txn):
        │   │       ├─ IncrementRefCount on each target hash
        │   │       ├─ coordinator.PersistFileChunks(target.Blocks, provisionalObjectID)
        │   │       ├─ DecrementRefCount on speculative-only hashes
        │   │       ├─ Cache.InvalidateFile(removedHashes)
        │   │       └─ local.DeleteAppendLog(payloadID)
        │   └─[hit] return Finalized:true (zero new CAS PUTs)
        │
        └─[post-Flush hook (on miss OR no trigger)]
            ├─ drainPayloadToRemote (uploadOne per Pending block)
            ├─ snapshotBlockRefs (every block now Remote)
            └─ persistFileChunksAfterFlush
                └─ ComputeObjectID(blocks)
                └─ coordinator.PersistFileChunks(blocks, objectID)
                    └─ runtime coordinator: WithTransaction(GetFileByPayloadID + PutFile)
                        // FileAttr.Blocks AND FileAttr.ObjectID
                        // written in one metadata txn
```

Both branches finalize `FileAttr.ObjectID` inside the same metadata
transaction that persists `FileAttr.Blocks`. The hit branch
performs zero new CAS PUTs (donor blocks already exist remotely);
the miss branch uploads each Pending block once via `uploadOne` and
then runs the post-Flush hook.

Source-of-truth file:line anchors:

- `pkg/block/engine/syncer.go::Flush` — entry point + branch
  selection; `snapshotPendingBlockRefs` (short-circuit input) and
  `snapshotBlockRefs` (post-Flush input) helpers.
- `pkg/block/engine/dedup.go::TrySpeculativeFileLevelDedup` and
  `applyFileLevelDedupHit` — the metadata-side swap.
- `pkg/block/engine/dedup.go::persistFileChunksAfterFlush` — the
  post-Flush coordinator hook.
- `pkg/controlplane/runtime/shares/coordinator.go::PersistFileChunks` /
  `GetFileObjectID` — runtime forwarders.

### Concurrent quiesce: first-committer-wins

Two concurrent flushes of byte-identical content race independently
(no distributed locking). At commit time the partial unique index on
`object_id` ensures exactly one write succeeds; the loser detects the
conflict (Postgres SQLSTATE `23505` / `metadata.ErrConflict` on Memory
and Badger), decrements its just-uploaded refs, swaps to the now-
existing target's BlockRef list, and re-commits. One wasted upload
per loser is acceptable; GC reclaims any orphans. See
`pkg/metadata/storetest/objectid_lookup.go` for the cross-backend
race conformance scenarios.

### Per-backend ObjectID lookup index

`MetadataStore.FindByObjectID(ctx, ObjectID) ([]BlockRef, error)`
returns `(nil, nil)` on miss; on hit returns the canonical BlockRef
list of the matching file (per-metadata-store scope, NOT per-share).
Backends maintain a secondary index:

| Backend  | Index                                                                       |
|----------|-----------------------------------------------------------------------------|
| Postgres | Partial unique: `inodes_object_id_idx ON inodes(object_id) WHERE object_id IS NOT NULL` |
| SQLite   | Partial unique: `inodes_object_id_idx ON inodes(object_id) WHERE object_id IS NOT NULL` (pure-Go `glebarez/go-sqlite`, mirrors the Postgres model) |
| Badger   | Secondary key `obj:{hex} -> file_id`, maintained inside each `Put`/`Delete` write batch |
| Memory   | `map[ContentHash]uuid`, guarded by the existing store mutex                 |

Zero-valued ObjectID (legacy / pre-quiesce) is excluded from the index
— `FindByObjectID(zero)` short-circuits to `(nil, nil)` at every layer
so partial states never trigger a false short-circuit.

### Observability

The dedup path emits slog-only signals:

- **DEBUG**: post-Flush ObjectID persisted; short-circuit hit/miss
  with `payloadID`, `objectID`, `donor_blocks`.
- **INFO**: cross-VM dedup ratio emitted by the e2e fixture
  (`test/e2e/dedup_vmfleet_test.go`, nightly).

### Performance gate

A CI perf lane gates random-write regression against a baseline
(`pkg/block/engine/perf_bench_test.go`). ObjectID compute is one
BLAKE3 pass over `32×N` bytes per quiesce (sub-millisecond at N=16K
BlockRefs); the short-circuit lookup is one indexed query per quiesce.
Both fire off the random-write hot path.

## Migration & Block-Layout Routing

DittoFS has had three block layouts (see the migration guide's table). Two
transitions are handled at startup, per share, before the share serves.

### Standalone CAS (v0.16-v0.21) → packed blocks: automatic

The current layout packs chunks into `blocks/<id>` container objects. A share
carrying leftover standalone-CAS state — pre-flip per-chunk local files, remote
`cas/` objects, or chunk locators that still point at standalone objects — is
converted at `engine.Store.Start`, blocking until done, by
`engine.Store.migrateLegacyCAS` (`pkg/block/engine/legacy_migration.go`):

1. **Phase L** imports pre-flip per-chunk local files into the local journal
   (BLAKE3-verified, deduplicated) and deletes them
   (`fs.FSStore.MigrateLegacyChunkFiles`).
2. **Phase R** re-packs every chunk whose synced marker still carries a
   standalone locator into `blocks/<id>` objects. Each block's record and all
   its chunk-locator rewrites commit in **one metadata transaction**
   (`metadata.DefaultCommitBlock`, last-wins locator overwrite), so a crash can
   never leave a block record pointing at only some of its chunks.
3. **Phase P** purges the now-unreferenced `cas/` namespace.

The migration is idempotent and resumable: a killed run converges on the next
start (a crash between PutBlock and the commit leaves at most one orphan block
object — the same class the live carver produces, reclaimed by the reconcile
sweep — never a leaked record). Detection is state-free: an `EnumerateSynced`
scan for standalone locators plus one remote LIST page. The legacy standalone
layout is understood ONLY by this routine and the `remote.LegacyCASStore`
accessors it drives; the live read path refuses a standalone locator as
post-migration drift. If a share's remote is unreachable while standalone
chunks remain, that share fails to start (its data would be unreadable anyway).

### Pre-v0.16 `.blk` → CAS: migrate with dittofs ≤ v0.21

The offline `.blk`→CAS tool (`dfs migrate-to-cas`) shipped through v0.21 and
has been removed. `newFSStore` still probes each share for the legacy `.blk`
layout on open (a `.cas-migrated-v1` sentinel from an old run short-circuits
the probe) and returns `block.ErrLegacyLayoutDetected`; the boot guard in
`cmd/dfs/commands/start.go` unwraps it, prints a directive to migrate with an
earlier release, and exits 78 (`EX_CONFIG`). After that migration + upgrade,
the automatic cas→blocks conversion above finishes the job.

See [the migration guide](../guide/block-store-migration.md) for the operator
runbook.

## Performance Characteristics

DittoFS is designed for high performance through several architectural choices:

- **Direct protocol implementation**: No FUSE overhead
- **Goroutine-per-connection model**: Leverages Go's lightweight concurrency
- **Buffer pooling**: Reduces GC pressure for large I/O operations
- **Streaming I/O**: Efficient handling of large files without full buffering
- **Three-tier storage**: Unified CAS-keyed `Cache` + local disk + remote store for optimal read latency
- **Zero-copy aspirations**: Working toward minimal data copying in hot paths

## Why Pure Go?

Go provides significant advantages for a project like DittoFS:

- **Easy deployment**: Single static binary, no runtime dependencies
- **Cross-platform**: Native support for Linux, macOS, Windows
- **Easy integration**: Embed DittoFS directly into existing Go applications
- **Modern concurrency**: Goroutines and channels for natural async I/O
- **Memory safety**: No buffer overflows or use-after-free vulnerabilities
- **Strong ecosystem**: Rich standard library and third-party packages
- **Fast compilation**: Quick iteration during development
- **Built-in tooling**: Testing, profiling, and race detection included
