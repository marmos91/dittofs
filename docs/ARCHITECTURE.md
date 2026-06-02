# DittoFS Architecture

This document provides a deep dive into DittoFS's architecture, design patterns, and internal implementation.

**Storage terms used throughout** (see the [Glossary](GLOSSARY.md) for protocol and security terms):

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
│     Stores     │  │  pkg/blockstore/     │
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

**6. BlockStore** (`pkg/blockstore/`)
- Per-share block storage orchestrator. Each share gets its own `*engine.BlockStore` instance.
- `engine.BlockStore` composes `local.LocalStore + remote.RemoteStore + engine.Syncer`
- Each share gets an isolated local storage directory; remote stores can be shared across shares (ref counted)
- `shares.Service` owns the lifecycle (create on AddShare, close on RemoveShare)
- Sub-packages:
  - `engine/`: BlockStore orchestrator — composes local + remote stores and owns the unified CAS-keyed `Cache` (read buffering + prefetch), the syncer, and the garbage collector. See `pkg/blockstore/engine/cache.go` for the Cache type.
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
- **Remote Sharing**: Multiple shares can reference the same remote store (e.g., same S3 bucket). The remote keyspace is content-addressed (`cas/{hh}/{hh}/{hex}`), so identical chunks dedup across every share that targets the same bucket+prefix. For isolation, give shares different buckets or prefixes
- **Lifecycle Independence**: Block stores are created/closed with share lifecycle

## Storage Tiers

DittoFS uses a three-tier storage model for block data:

```
┌─────────────────────────────────────┐
│  Cache (In-Memory, CAS-keyed)       │
│  pkg/blockstore/engine/cache.go     │
│  - Single type, keyed by ContentHash│
│  - LRU eviction                     │
│  - Internal sequential prefetch     │
│    (3-trigger threshold)            │
│  - Cross-file dedup                 │
│  - Configurable budget per share    │
│    (cache.size_mib, default 256)    │
│  - Volatile (lost on restart)       │
└──────────────┬──────────────────────┘
               │ cache miss
               ▼
┌─────────────────────────────────────┐
│  Local Block Store                  │
│  pkg/blockstore/local/fs/           │
│  - Filesystem-backed                │
│  - Fast access (disk I/O)           │
│  - Persistent across restarts       │
│  - Per-share isolated directories   │
└──────────────┬──────────────────────┘
               │ block not local
               ▼
┌─────────────────────────────────────┐
│  Remote Store                       │
│  pkg/blockstore/remote/s3/          │
│  - S3 or compatible object store    │
│  - Slowest (network I/O)            │
│  - Durable (survives node loss)     │
│  - Shared across shares (ref count) │
└─────────────────────────────────────┘
```

**Read Path**: Engine.ReadAt receives `[]BlockRef` from caller, locates the
covering blocks via `findBlocksForRange` (binary search), serves bytes
from local CAS (mmap on linux/darwin, ReadFile on windows)
or remote CAS (BLAKE3-verified end-to-end), calls `Cache.OnRead`
to update the per-payload sequential tracker for prefetch hints.

**Write Path**: Engine.WriteAt receives `(currentBlocks []BlockRef, data,
offset)`, FastCDC-rechunks the affected range, returns `newBlocks
[]BlockRef` to the caller; caller persists newBlocks alongside the
metadata transaction (Mtime, Size, etc.). Syncer asynchronously uploads
Pending FileBlocks to remote CAS.

**Eviction**:
- Cache: LRU eviction when budget reached. No data loss (local CAS has the data). Cache is per-share but cross-file inside a share — the same hash referenced by two files shares one entry.
- Local store: Manual eviction via `dfsctl store block evict`. Only blocks already synced to remote can be evicted (safety check prevents data loss).

## Block Store -- Local Append-Log Tier

The local filesystem store (`pkg/blockstore/local/fs/`) writes through an
append-only log per file. A rollup pool chunks the log via FastCDC, hashes
each chunk with BLAKE3, and persists the chunks under a content-addressable
`blocks/{hh}/{hh}/{hex}` directory. The syncer then uploads those chunks to
the remote content-addressable keyspace (`cas/{hh}/{hh}/{hex}`), and a
mark-sweep GC reclaims the remote `cas/` prefix.

This is the only local write path. Servers from v0.16 on require the CAS
layout; a store directory still holding the older `.blk` layout is detected
on open and the operator is told to run `dfs migrate-to-cas` (see
[Migration & Block-Layout Routing](#migration--block-layout-routing)).

See [Block Lifecycle (three-state)](#block-lifecycle-three-state) and
[Garbage Collection (mark-sweep)](#garbage-collection-mark-sweep) below.

### Pipeline

```
                                                       (log header + records)
                                                       logs/{payloadID}.log
  AppendWrite ---> per-file log (append-only)  ---------------+
  (per-file mutex)   CRC per record                           |
                                                              v
                                                       chunkRollup pool
                                                       (default 2 workers)
                                                              |
                                       BLAKE3 + FastCDC       |
                                       (min 1 MiB / avg 4 MiB / max 16 MiB)
                                                              |
                                                              v
                                                       StoreChunk
                                                       blocks/{hh}/{hh}/{hex}
                                                       (.tmp + rename + fsync)
                                                              |
                                        CommitChunks atomic:  |
                                         1. metadata.SetRollupOffset (source of truth)
                                         2. advanceRollupOffset + fsync log header
                                         3. tree.ConsumeUpTo + logBytesTotal.Sub
                                         4. non-blocking signal on pressureCh
                                                              |
                                                              v
                                                       (blocked AppendWrite unblocks)
```

### Layout

```
<baseDir>/logs/<payloadID>.log        per-file append-only log
<baseDir>/blocks/<hh>/<hh>/<hex>      content-addressed chunks (CAS)
```

Log header (64 bytes): magic `DFLG` | version | `rollup_offset` | flags |
`created_at` | header CRC | 32 B reserved. Record framing:
`payload_len` (u32 LE) | `file_offset` (u64 LE) | `crc32c` (u32 LE) |
payload.

### Invariants

- **`rollup_offset` is monotone:** metadata is the source of truth; the
  filesystem header is idempotent derived state. Recovery reconciles the
  header from metadata on boot.
- **Log length is bounded:** `logBytesTotal <= max_log_bytes` per
  `FSStore`. Writers block on `pressureCh` when the budget is exceeded;
  rollup drains and non-blocking signals when bytes are reclaimed.

### Crash recovery

Recovery (`pkg/blockstore/local/fs/recovery.go`) scans logs from
`rollup_offset`, truncates at first bad CRC, and rebuilds per-file interval
trees. Orphan logs (no metadata referrer, no live FileBlock, mtime older
than `orphan_log_min_age_seconds`) are swept. Orphan chunks under
`blocks/{hh}/{hh}/{hex}` are reclaimed by the mark-sweep GC.

### Per-`FSStore` surface

Because block stores are per-share (see the invariants in `CLAUDE.md`),
every local-tier field -- log-fd map, per-file mutex map, interval-tree
map, rollup worker pool, pressure channel, `maxLogBytes` budget,
stabilization window -- lives inside `*FSStore`. No global state across
shares.

See `docs/CONFIGURATION.md` (`max_log_bytes`, `rollup_workers`,
`stabilization_ms`, `orphan_log_min_age_seconds`) for the tunables.

## Block Lifecycle (three-state)

The block lifecycle has three persisted states held on `FileBlock.State`
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
- **Remote**: PUT to the remote CAS keyspace returned 200 AND the
  metadata transaction setting `State=Remote` committed (no orphan flag
  without metadata-txn success).

**Restart recovery:** at syncer Start, a one-shot janitor pass
requeues any `Syncing` row whose `last_sync_attempt_at` is older than
`syncer.claim_timeout` (default 10m) back to `Pending`. CAS keys are
content-defined so a duplicate re-upload writes the same bytes to the
same key — idempotent by construction.

**Why a metadata write for every claim?** The Pending → Syncing
transition is the serialization point against duplicate uploads across
syncer instances. The batched-claim cost is one txn per tick, in exchange
for exact restart recovery and a single-query introspection of stuck
blocks (`State=Syncing AND last_sync_attempt_at < now − 1h`).

## Garbage Collection (mark-sweep)

The block-store GC is a fail-closed mark-sweep over the union of every live
`FileBlock.ContentHash` across all shares pointing at the same remote.

### Algorithm

1. **Mark phase.** Stream every `FileBlock`'s `ContentHash` via the
   `MetadataStore.EnumerateFileBlocks(ctx, fn)` cursor. The cursor
   is implemented natively per backend (memory, Badger, Postgres) and
   never loads the full set into application memory. Hashes are appended
   to an on-disk live set under `<localStore>/gc-state/<runID>/db/`
   (a Badger temp store). Snapshot time `T` is captured at the start of
   the run. Cross-share aggregation keys on **remote-store identity**
   (`bucket+endpoint+prefix`), not share name, so an object reachable from
   any share that targets the same remote is considered live.
2. **Sweep phase.** A single `RemoteStore.Walk` enumerates every CAS
   object cluster-wide; the backend (e.g. S3) paginates internally. For
   each key, the engine keeps the object iff the hash is present in the
   live set OR the object's `LastModified` is newer than
   `T − gc.grace_period` (default 1h). Otherwise the engine issues a DELETE.

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

See [SNAPSHOTS.md](SNAPSHOTS.md#10-gc-hold-semantics) for the
operator-facing description of the hold semantics, including the
delete-vs-GC race window.

See `docs/CONFIGURATION.md` for every `gc.*` and `syncer.*` knob, and
`docs/CLI.md` for the `dfsctl store block gc` reference.

## Share Snapshots

Share snapshots are point-in-time, reference-based protection for a
share's content. The subsystem produces three artifacts per snapshot
on local disk and one row in the control-plane database; it does not
copy any block data. See [SNAPSHOTS.md](SNAPSHOTS.md) for the
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
[SNAPSHOTS.md](SNAPSHOTS.md).

## Block Reads (content-addressable)

The engine resolves every block read from the content-addressable keyspace:
read from `cas/{hh}/{hh}/{hex}`, BLAKE3-verified end-to-end (a header
pre-check on `x-amz-meta-content-hash` plus a streaming verifier over the
body). Resolution is by metadata key — one DB lookup per block — not by
remote trial-and-error, so there is no doubled GET cost.

The older non-CAS layout (`{payloadID}/block-{N}`) is no longer read at
runtime. A store directory still on that layout is detected on open and the
operator is directed to run `dfs migrate-to-cas`, which re-chunks all data
into the CAS keyspace. See
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
err = metadataStore.SetFileBlocks(handle, newBlocks, authCtx)

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

See [docs/IMPLEMENTING_STORES.md](IMPLEMENTING_STORES.md) for detailed implementation guides for:
- **Local Store**: Implement `pkg/blockstore/local.LocalStore` interface
- **Remote Store**: Implement `pkg/blockstore/remote.RemoteStore` interface
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
│   │   ├── store.go              # FileBlockStore interface
│   │   ├── types.go              # FileBlock, BlockState types
│   │   ├── errors.go             # BlockStore error types
│   │   ├── chunker/              # FastCDC content-defined chunker
│   │   │                         # min=1 MiB / avg=4 MiB / max=16 MiB, lvl 2;
│   │   │                         # BLAKE3 hashing; consumed by local rollup pool
│   │   ├── engine/               # BlockStore orchestrator + read cache + syncer + GC
│   │   ├── local/                # Local store interface
│   │   │   ├── fs/               # Filesystem-backed local store
│   │   │   │                     # (append-log + CAS blocks/ tier)
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
defined in `pkg/blockstore/types.go`. `FileAttr.Blocks []BlockRef` (in
`pkg/metadata/file_types.go`) is the authoritative, offset-sorted list of
every chunk that composes a file. It is populated on every sync
finalization; the engine binary-searches it via `findBlocksForRange`
(`pkg/blockstore/engine/range.go`).

Storage encodings differ per backend:

- **Postgres** uses a separate `file_block_refs` table with PK
  `(file_id, offset) INCLUDE (size, hash)`, FK `ON DELETE CASCADE`, hash
  column `BYTEA`. Random 4 KiB writes touch 1–2 rows instead of rewriting a
  ~1.5 MB TOAST blob.
- **Badger** and **Memory** inline-encode `Blocks []BlockRef` inside the
  existing `FileAttr` blob (gob for Badger, typed structs for Memory).

### Engine API

```go
// pkg/blockstore/engine/engine.go
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
`FileBlock.RefCount` for every distinct hash in `srcBlocks` and inserts
the dst rows. No data copy. This is the file-level dedup primitive the
ObjectID layer (below) builds on.

`MetadataCoordinator` (`pkg/blockstore/engine/coordinator.go`) is the
narrow interface the engine uses to mutate refcounts and persist
`FileAttr.Blocks`. The engine never opens a metadata txn itself — a
strict-grep build gate enforces zero `pkg/metadata` imports under
`pkg/blockstore/engine/*.go` production files except a single justified
exception in `gc.go`.

### Cache

The `Cache` type (`pkg/blockstore/engine/cache.go`) is keyed solely by
`ContentHash`. It combines read buffering and prefetch into a single
per-share type with a single budget (`cache.size_mib`, default 256 MiB).
Two files reading the same chunk hit the same entry (cross-file dedup).

```go
// pkg/blockstore/engine/cache.go (hint API)
OnRead(payloadID PayloadID, hashes []ContentHash, fileSize uint64)
InvalidateFile(payloadID PayloadID, removedHashes []ContentHash)  // surgical
```

Sequential prefetch triggers after 3 consecutive sequential reads (to
suppress speculative prefetch on accidental two-block runs in random-IO
workloads). Bounded concurrency: 4 worker goroutines per cache by default.
LRU eviction.

Single-copy reads: on Linux/Darwin, `readFromCAS` (`cache_mmap_unix.go`)
`mmap`s the local CAS chunk and `copy(dest, mapped[offset:])` once. Chunks
below 64 KiB use `os.ReadFile` (mmap setup overhead dominates tiny reads).
Windows uses `os.ReadFile` only.

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
  reconciliation audit (`∑ FileBlock.RefCount == ∑ len(FileAttr.Blocks)`),
  emits aggregate counts as structured slog INFO, and persists the
  last-run summary at `<localStore>/audit-state/last-inv02.json`. See
  `docs/CLI.md` for the full reference and `docs/FAQ.md` for operator
  guidance.
- Cache and prefetch knobs (`cache.size_mib`, `cache.prefetch_threshold`,
  `cache.prefetch_max_depth`, `cache.prefetch_workers`) are documented
  in `docs/CONFIGURATION.md`.

## File-Level Dedup: ObjectID + Merkle Root

File-level dedup layers on top of the chunk-level CAS path. Each
`FileAttr` carries an `ObjectID` — a BLAKE3
Merkle root computed over the file's `BlockRef.Hash` values sorted by
`Offset`, prefixed by the domain-separation tag
`dittofs:objectid:v1\x00`:

    ObjectID = BLAKE3("dittofs:objectid:v1\x00" || h0 || h1 || ... || hN-1)

Implemented in `blockstore.ComputeObjectID`
(`pkg/blockstore/objectid.go`). Stable across rename and engine restart
by construction (BLAKE3 + FastCDC are both deterministic; the prefix
protects the output space from per-chunk hash collisions and reserves
room for future input-shape changes via `v2`/`v3`).

### Lifecycle

- **Cleared (zeroed)** on first dirty write that mutates `FileAttr.Blocks`,
  in the same metadata transaction.
- **Recomputed and persisted** at the post-Flush coordinator hook
  (`Syncer.persistFileBlocksAfterFlush` → `MetadataCoordinator.PersistFileBlocks`),
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
        │   ├─ snapshotPendingBlockRefs(payloadID)         // ListFileBlocks projection
        │   ├─ coordinator.GetFileObjectID(payloadID)      // trigger-condition check
        │   ├─ TrySpeculativeFileLevelDedup
        │   │   ├─ ComputeObjectID(specBlocks)
        │   │   ├─ coordinator.FindByObjectID
        │   │   └─ applyFileLevelDedupHit (one metadata txn):
        │   │       ├─ IncrementRefCount on each target hash
        │   │       ├─ coordinator.PersistFileBlocks(target.Blocks, provisionalObjectID)
        │   │       ├─ DecrementRefCount on speculative-only hashes
        │   │       ├─ Cache.InvalidateFile(removedHashes)
        │   │       └─ local.DeleteAppendLog(payloadID)
        │   └─[hit] return Finalized:true (zero new CAS PUTs)
        │
        └─[post-Flush hook (on miss OR no trigger)]
            ├─ drainPayloadToRemote (uploadOne per Pending block)
            ├─ snapshotBlockRefs (every block now Remote)
            └─ persistFileBlocksAfterFlush
                └─ ComputeObjectID(blocks)
                └─ coordinator.PersistFileBlocks(blocks, objectID)
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

- `pkg/blockstore/engine/syncer.go::Flush` — entry point + branch
  selection; `snapshotPendingBlockRefs` (short-circuit input) and
  `snapshotBlockRefs` (post-Flush input) helpers.
- `pkg/blockstore/engine/dedup.go::TrySpeculativeFileLevelDedup` and
  `applyFileLevelDedupHit` — the metadata-side swap.
- `pkg/blockstore/engine/dedup.go::persistFileBlocksAfterFlush` — the
  post-Flush coordinator hook.
- `pkg/controlplane/runtime/shares/coordinator.go::PersistFileBlocks` /
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
| Postgres | Partial unique: `files_object_id_idx ON files(object_id) WHERE object_id IS NOT NULL` (migration `000013_object_id`) |
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
(`pkg/blockstore/engine/perf_bench_test.go`). ObjectID compute is one
BLAKE3 pass over `32×N` bytes per quiesce (sub-millisecond at N=16K
BlockRefs); the short-circuit lookup is one indexed query per quiesce.
Both fire off the random-write hot path.

## Migration & Block-Layout Routing

`dfs migrate-to-cas` is the offline tool that converts a share's block
layout from the older path-indexed keys (`{payloadID}/block-{idx}`) to the
content-addressable layout (`cas/{hh}/{hh}/{hex}`). Two pieces support it:
a per-share **`block_layout`** flag, and an engine-level gate that fails
loud on legacy reads once a share is marked CAS-only.

### Per-share `block_layout` flag

A field `block_layout` on `metadata.ShareOptions` carries the share's
authoritative layout state:

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

Storage:

| Backend  | Layout                                                               |
|----------|----------------------------------------------------------------------|
| Postgres | Dedicated `block_layout TEXT NOT NULL DEFAULT 'legacy'` column on `shares` (reversible migration). Authoritative over the options JSON blob. |
| Badger   | Inline-encoded inside the existing `ShareOptions` blob (gob; `omitempty` on the field for forward-compat with older rows). |
| Memory   | Direct field on the in-process struct.                               |

`ParseBlockLayout("")` coerces empty / missing values to
`BlockLayoutLegacy` so older metadata rows decode cleanly. Unknown values
surface `metadata.ErrInvalidBlockLayout` rather than silently coercing.

The flag is read **once** by `shares.Service.createBlockStoreForShare`
when the share's per-share `*engine.BlockStore` is constructed, then
threaded into `engine.SyncerConfig.BlockLayout`. The engine never
re-reads it during normal operation; the migration tool's cutover
runs while the daemon is offline so a stale in-memory copy is
impossible.

### The CAS-only gate

A share marked `block_layout=cas-only` must never read from the older
key space. The gate that enforces this sits in
`engine.Syncer.dispatchRemoteFetch`:

```text
        ┌───────────────────────────────────────────┐
        │ engine.Syncer.dispatchRemoteFetch(block)  │
        └────────────────────┬──────────────────────┘
                             │
                             ▼
              block.Hash != ZeroContentHash ?
                ┌────────────┴────────────┐
              yes (CAS shape)            no (legacy shape)
                │                          │
                ▼                          ▼
       remote.ReadBlockVerified     [BlockLayout gate]
                │                          │
                │                ┌─────────┴─────────┐
                │             legacy              cas-only
                │                │                  │
                ▼                ▼                  ▼
          (CAS path)     remote.ReadBlock    ErrLegacyReadOnCASOnly
                                             (fail loud, slog Error)
```

On a `cas-only` share, a legacy-shaped FileBlock surfaces
`engine.ErrLegacyReadOnCASOnly`: the function logs at Error with
`block_id` + `store_key` and returns the wrapped sentinel rather than
silently falling through to `ReadBlock`. This guards against a
freshly-migrated share encountering a forgotten legacy FileBlock — the
engine fails loud rather than reading from a key the migration already
deleted.

The gate is defense-in-depth: the migration's atomic per-file `PutFile`
already updates every legacy FileBlock to the CAS shape before flipping
`block_layout`. A legacy-shaped block post-cutover indicates a migration
bug, metadata corruption, or a hand-edited row — all of which demand
operator attention rather than a silent legacy read.

### The migration tool

`dfs migrate-to-cas` is intentionally **offline-only** and runs against the
stopped server's storage root:

- It requires `--storage-dir <root>`, expected to contain a
  `shares/<name>/blocks/` subtree per share.
- It refuses to run while a daemon is serving the target share.
- It is idempotent: a per-share journal at
  `<storage-dir>/shares/<name>/.dittofs-migrate-to-cas.state` lets a run
  resume after a crash without re-uploading already-migrated chunks.
- The pipeline is: walk → FastCDC re-chunk → `GetByHash` dedup probe →
  upload (or `IncrementRefCount`) → `PutFile` Blocks + ObjectID → journal
  append → integrity HEAD-per-ref → cutover (`block_layout` flip) → legacy
  delete sweep.
- On success it writes `<storage-dir>/shares/<name>/.cas-migrated-v1` via
  atomic rename; the server's boot guard refuses to start until that
  sentinel exists.

See [BLOCKSTORE_MIGRATION.md](BLOCKSTORE_MIGRATION.md) for the full
operator runbook.

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
