# DittoFS Architecture

This document provides a deep dive into DittoFS's architecture, design patterns, and internal implementation.

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

**6. BlockStore** (`pkg/blockstore/`)
- Per-share block storage orchestrator. Each share gets its own `*engine.BlockStore` instance.
- `engine.BlockStore` composes `local.LocalStore + remote.RemoteStore + engine.Syncer`
- Each share gets an isolated local storage directory; remote stores can be shared across shares (ref counted)
- `shares.Service` owns the lifecycle (create on AddShare, close on RemoveShare)
- Sub-packages:
  - `engine/`: BlockStore orchestrator — composes local + remote stores and owns the read cache, syncer, prefetcher, and garbage collector (merged from former `readbuffer/`, `sync/`, `gc/` packages per TD-01)
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
- **Read Buffer Independence**: Read buffer is per-share (eviction in one share does not affect others)
- **Remote Sharing**: Multiple shares can reference the same remote store (e.g., same S3 bucket) -- blocks are namespaced by share to prevent collisions
- **Lifecycle Independence**: Block stores are created/closed with share lifecycle

## Storage Tiers

DittoFS uses a three-tier storage model for block data:

```
┌─────────────────────────────────────┐
│  Read Buffer (In-Memory)            │
│  pkg/blockstore/engine/ (cache)     │
│  - LRU eviction                     │
│  - Fastest access (nanoseconds)     │
│  - Volatile (lost on restart)       │
│  - Configurable memory limit        │
│  - Prefetch for sequential reads    │
└──────────────┬──────────────────────┘
               │ buffer miss
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

**Read Path**: Read buffer hit -> return. Buffer miss -> local hit -> populate buffer, return. Local miss -> remote fetch -> store locally, populate buffer, return.

**Write Path**: Write to local store. Syncer asynchronously uploads to remote store. Read buffer is populated on subsequent reads.

**Eviction**:
- Read buffer: LRU eviction when memory limit reached. No data loss (local store has the data).
- Local store: Manual eviction via `dfsctl store block evict`. Only blocks already synced to remote can be evicted (safety check prevents data loss).

## Block Store -- Hybrid Local Tier (experimental, v0.15.0 Phase 10)

The hybrid local tier is a second write path inside `pkg/blockstore/local/fs/`,
gated by the `use_append_log` flag (defaults to `false` through v0.15.0
Phase 10; flipped to `true` in Phase 11). When enabled, writes flow through
an append-only log per file; a rollup pool chunks the log via FastCDC,
hashes each chunk with BLAKE3, and persists the chunks under a
content-addressable `blocks/{hh}/{hh}/{hex}` directory.

**Phase 10 is plumbing-only.** No existing write path consumes the chunker
or the log in v0.15.0 Phase 10; the engine keeps using the legacy
`tryDirectDiskWrite` / `.blk` path. Phase 11 (A2) flips the default,
rewires the syncer to write to the remote CAS keyspace
(`cas/{hh}/{hh}/{hex}`), and adds mark-sweep GC for the remote `cas/`
prefix. See [Garbage Collection (mark-sweep)](#garbage-collection-mark-sweep-v0150-phase-11)
and [Block Lifecycle (three-state)](#block-lifecycle-three-state-v0150-phase-11)
below for the v0.15.0 Phase 11 design that consumes this tier.

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

- **INV-03** (`rollup_offset` monotone): metadata is source of truth; the
  filesystem header is idempotent derived state. Recovery reconciles header
  from metadata on boot.
- **INV-05** (log length bounded): `logBytesTotal <= max_log_bytes` per
  `FSStore`. Writers block on `pressureCh` when the budget is exceeded;
  rollup drains and non-blocking signals when bytes are reclaimed.

### Crash recovery

Recovery (`pkg/blockstore/local/fs/recovery.go`) scans logs from
`rollup_offset`, truncates at first bad CRC, and rebuilds per-file interval
trees. Orphan logs (no metadata referrer, no live FileBlock, mtime older
than `orphan_log_min_age_seconds`) are swept. Orphan chunks under
`blocks/{hh}/{hh}/{hex}` are left intact; Phase 11's mark-sweep GC is what
reclaims them.

### Per-`FSStore` surface

Per CLAUDE.md Rule 4 (block stores are per-share), every hybrid-tier field
-- log-fd map, per-file mutex map, interval-tree map, rollup worker pool,
pressure channel, `maxLogBytes` budget, stabilization window -- lives
inside `*FSStore`. No global state across shares.

**Experimental:** Do not enable `use_append_log` in production before
v0.15.0 Phase 11 (A2). Without Phase 11's mark-sweep GC, the `blocks/`
directory grows unbounded. See `docs/CONFIGURATION.md` (`use_append_log`,
`max_log_bytes`, `rollup_workers`, `stabilization_ms`,
`orphan_log_min_age_seconds`) and
`.planning/phases/10-fastcdc-chunker-hybrid-local-store-a1/10-CONTEXT.md`
for full design detail.

## Block Lifecycle (three-state, v0.15.0 Phase 11)

Phase 11 (A2) collapses the block lifecycle to three persisted states held
on `FileBlock.State` indexed by `ContentHash`. There is no parallel state
in memory, in fd pools, or anywhere else (STATE-03): the metadata store
is the single source of truth, and `engine.Syncer` is the sole owner of
state transitions (D-15).

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
- **Syncing**: a syncer goroutine has claimed the block (batched per
  `syncer.claim_batch_size`, default 32); the upload is in flight.
- **Remote**: PUT to the remote CAS keyspace returned 200 AND the
  metadata transaction setting `State=Remote` committed (INV-03 — no
  orphan flag without metadata-txn success).

**Restart recovery (D-14):** at syncer Start, a one-shot janitor pass
requeues any `Syncing` row whose `last_sync_attempt_at` is older than
`syncer.claim_timeout` (default 10m) back to `Pending`. CAS keys are
content-defined so a duplicate re-upload writes the same bytes to the
same key — idempotent by construction.

**Why a metadata write for every claim?** The Pending → Syncing
transition is the serialization point against duplicate uploads across
syncer instances. With `claim_batch_size=32` the cost is a single
batched txn per tick, in exchange for exact restart recovery and a
single-query introspection of stuck blocks (`State=Syncing AND
last_sync_attempt_at < now − 1h`).

## Garbage Collection (mark-sweep, v0.15.0 Phase 11)

Phase 11 replaces the previous path-prefix GC with a fail-closed
mark-sweep over the union of every live `FileBlock.ContentHash` across
shares pointing at the same remote.

### Algorithm

1. **Mark phase.** Stream every `FileBlock`'s `ContentHash` via the new
   `MetadataStore.EnumerateFileBlocks(ctx, fn)` cursor (D-02). The cursor
   is implemented natively per backend (memory, Badger, Postgres) and
   never loads the full set into application memory. Hashes are appended
   to an on-disk live set under `<localStore>/gc-state/<runID>/db/`
   (Badger temp store; D-01). Snapshot time `T` is captured at the
   start of the run. Cross-share aggregation keys on **remote-store
   identity** (`bucket+endpoint+prefix`), not share name (D-03), so an
   object reachable from any share that targets the same remote is
   considered live.
2. **Sweep phase.** A bounded worker pool (default
   `gc.sweep_concurrency=16`, max 32) walks the 256 top-level
   `cas/{XX}/` prefixes in parallel (D-04). For each S3 key, the worker
   keeps the object iff the hash is present in the live set OR the
   object's `LastModified` is newer than `T − gc.grace_period` (default
   1h, D-05). Otherwise the worker issues a DELETE.

### Fail-closed posture (INV-04)

Mark-phase and sweep-phase failures are treated asymmetrically (D-06,
D-07):

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
Mark is idempotent so resume-on-restart is intentionally not built —
simpler test surface (D-01).

### Triggers and observability

- **Periodic GC is deferred to a follow-up phase.** `gc.interval` is
  parsed and validated but unwired in v0.15.0; any non-zero value emits
  a startup WARN and is otherwise ignored. Schedule via cron until the
  scheduler ships.
- **On-demand** via `dfsctl store block gc <share> [--dry-run]`
  (D-08, D-09); `--dry-run` skips DELETEs and prints up to
  `gc.dry_run_sample_size` candidate keys (default 1000).
- **Observability** via structured slog INFO at start/end with `run_id`,
  `hashes_marked`, `objects_swept`, `bytes_freed`, `duration_ms`,
  `error_count`, plus a persisted summary at
  `<localStore>/gc-state/last-run.json` (D-10). Inspect via
  `dfsctl store block gc-status <share>`. Prometheus metrics are
  intentionally deferred to a metrics phase (D-35).

GC is decoupled from any backup-hold protocol: Phase 08 deleted
`BackupHoldProvider`, and GC-04 forbids reintroducing one. The v0.16.0
atomic-backup design uses CAS immutability + manifest snapshots, which
need no hold protocol (D-17).

See `docs/CONFIGURATION.md` for every `gc.*` and `syncer.*` knob, and
`docs/CLI.md` for the `dfsctl store block gc` reference.

## Dual-Read Window (Phase 11 → Phase 14)

During the v0.15.0 → v0.15.x window, the engine resolves block reads
from two coexisting key spaces (D-21, D-22):

- **`FileBlock.Hash` non-zero** → CAS path: read from
  `cas/{hh}/{hh}/{hex}`, BLAKE3-verified end-to-end (header pre-check
  on `x-amz-meta-content-hash` + streaming verifier over the body,
  INV-06).
- **`FileBlock.Hash` zero** → legacy path: read from
  `{payloadID}/block-{N}` (`FormatStoreKey`/`ParseStoreKey`) with no
  verification (verification cannot be retroactively applied to data
  written before BSCAS-06).

Resolution is by metadata key shape (one DB lookup per block), NOT by
S3 trial-and-error — there is no doubled GET cost.

The legacy code path lives Phase 11 → Phase 14 (A5). Phase 14 ships
`dfsctl blockstore migrate` to re-chunk all legacy data to CAS; Phase
15 (A6) deletes the legacy path entirely. The dual-read code is
intentionally on a deletion clock — anyone touching it should know
its lifespan.

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
    // Read data via block store
    data, err := blockStore.ReadAt(ctx, contentID, offset, size)
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
- **Phase-12 `[]BlockRef` seam**: `common.ReadFromBlockStore`,
  `common.WriteToBlockStore`, and `common.CommitBlockStore` are the single
  edit points where Phase 12 (v0.15.0 A3 / META-01 + API-01) will feed
  resolved `[]BlockRef` into the engine. Handler code stays untouched;
  Phase 12's blast radius is confined to `common/`.
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
// 1. Update metadata (validates permissions, updates size/timestamps)
attr, preSize, preMtime, preCtime, err := metadataStore.WriteFile(handle, newSize, authCtx)

// 2. Resolve per-share block store from file handle
blockStore, err := rt.GetBlockStoreForHandle(ctx, handle)

// 3. Write actual data via per-share block store
err = blockStore.WriteAt(ctx, string(attr.PayloadID), data, offset)

// 4. Return updated attributes to client for cache consistency
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
│   │   ├── chunker/              # FastCDC content-defined chunker (Phase 10 A1)
│   │   │                         # min=1 MiB / avg=4 MiB / max=16 MiB, lvl 2;
│   │   │                         # BLAKE3 hashing; consumed by local rollup pool
│   │   ├── engine/               # BlockStore orchestrator + read cache + syncer + GC
│   │   ├── local/                # Local store interface
│   │   │   ├── fs/               # Filesystem-backed local store
│   │   │   │                     # (+ hybrid append-log + CAS blocks/ tier,
│   │   │   │                     #  gated by use_append_log, Phase 10 A1)
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
│   │   │                         # CommitBlockStore seams (Phase 12 entry
│   │   │                         # point for []BlockRef), consolidated
│   │   │                         # metadata.ErrorCode -> NFS3/NFS4/SMB
│   │   │                         # mapping table (errmap + content_errmap +
│   │   │                         # lock_errmap).
│   │   ├── resolve.go            # BlockStoreRegistry narrow interface +
│   │   │                         # ResolveForRead/Write
│   │   ├── read_payload.go       # Pooled BlockReadResult + ReadFromBlockStore
│   │   ├── write_payload.go      # WriteToBlockStore + CommitBlockStore seams
│   │   ├── errmap.go             # Struct-per-code table (NFS3/NFS4/SMB columns)
│   │   ├── content_errmap.go     # Block-store content error table (D-08 §2)
│   │   └── lock_errmap.go        # Lock-context error table (D-08 §3)
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

## Performance Characteristics

DittoFS is designed for high performance through several architectural choices:

- **Direct protocol implementation**: No FUSE overhead
- **Goroutine-per-connection model**: Leverages Go's lightweight concurrency
- **Buffer pooling**: Reduces GC pressure for large I/O operations
- **Streaming I/O**: Efficient handling of large files without full buffering
- **Three-tier storage**: Read buffer + local disk + remote store for optimal read latency
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
