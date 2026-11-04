# DittoFS Architecture

This document provides a deep dive into DittoFS's architecture, design patterns, and internal implementation.

**Storage terms used throughout** (see the [Glossary](../guide/glossary.md) for protocol and security terms):

- **CAS** (Content-Addressed Storage) — blocks are named by the hash of their contents rather than by location, so identical data is stored once and deduplicated automatically.
- **FastCDC** — a content-defined chunking algorithm that splits file data at content-based boundaries, so small edits only re-chunk the affected region ([FastCDC paper](https://www.usenix.org/conference/atc16/technical-sessions/presentation/xia)).
- **BLAKE3** — the fast cryptographic hash used to address CAS blocks and verify them end-to-end ([BLAKE3 spec](https://github.com/BLAKE3-team/BLAKE3-specs)).

## Table of Contents

- [Core Abstraction Layers](#core-abstraction-layers)
- [Data path](#data-path)
- [Adapter Pattern](#adapter-pattern)
- [Control Plane Pattern](#control-plane-pattern)
- [Service Layer](#service-layer)
- [Built-In and Custom Backends](#built-in-and-custom-backends)
- [Directory Structure](#directory-structure)
- [Multi-Node Metadata](#multi-node-metadata)
- [Durable Handle State Flow](#durable-handle-state-flow)
- [Migration & Block-Layout Routing](#migration--block-layout-routing)

## Core Abstraction Layers

DittoFS uses a **Runtime-centric architecture** where the Runtime is the single entrypoint for all operations. This design ensures that both persistent store and in-memory state stay synchronized.

```
┌─────────────────────────────────────────┐
│         Protocol Adapters               │
│            (NFS, SMB)                   │
│       pkg/adapter/{nfs,smb}/            │
└───────────────┬─────────────────────────┘
                │ metadata service (data path cut)
                ▼
┌─────────────────────────────────────────┐
│              Runtime                    │
│   (Composition layer + sub-services)    │
│   pkg/controlplane/runtime/             │
│                                         │
│  ┌──────────┐ ┌────────┐ ┌──────────┐  │
│  │ adapters │ │ stores │ │  shares  │  │
│  │lifecycle │ │registry│ │ registry │  │
│  └──────────┘ └────────┘ │ + remote │  │
│  ┌──────────┐ ┌────────┐ └──────────┘  │
│  │  mounts  │ │lifecycl│ ┌──────────┐  │
│  │ tracking │ │  serve  │ │ identity │  │
│  └──────────┘ └────────┘ │ mapping  │  │
│                           └──────────┘  │
│  ┌────────────┐  ┌───────────────────┐  │
│  │   Store    │  │   Auth Layer      │  │
│  │ (Persist)  │  │  pkg/auth/        │  │
│  │ 9 sub-ifs  │  │ kerberos,         │  │
│  │            │  │ sid               │  │
│  └────────────┘  └───────────────────┘  │
└───────┬───────────────────┬─────────────┘
        │                   │
        ▼                   ▼
┌────────────────┐  ┌──────────────────────┐
│   Metadata     │  │ Per-Share BlockStore │
│     Stores     │  │  pkg/block/     │
│                │  │                      │
│  - BadgerDB    │  │  ┌──────────────┐    │
│    (on disk or │  │  │   Journal    │    │
│     in memory) │  │  │  (on disk)   │    │
│                │  │  └──────┬───────┘    │
│                │  │         │            │
│                │  │  ┌──────▼───────┐    │
│                │  │  │   Syncer     │    │
│                │  │  │ (async xfer) │    │
│                │  │  └──────┬───────┘    │
│                │  │         │            │
│                │  │  ┌──────▼────────┐   │
│                │  │  │  Block Store  │   │
│                │  │  │  s3 / memory  │   │
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

**2. Control Plane Store** (`pkg/controlplane/store/`)
- Persistent configuration (users, groups, permissions, adapters)
- Decomposed into 9 sub-interfaces: `UserStore`, `GroupStore`, `ShareStore`, `PermissionStore`, `MetadataStoreConfigStore`, `BlockStoreConfigStore`, `AdapterStore`, `SettingsStore`, `GuestStore`
- Composite `Store` interface embeds all sub-interfaces
- API handlers accept narrowest interface needed
- SQLite (single-node) or PostgreSQL (distributed)

**3. Adapter Interface** (`pkg/adapter/adapter.go`)
- Each protocol implements the `Adapter` interface (`Serve`, `Stop`, `SetRuntime`, `Protocol`, `Port`, `Healthcheck`)
- Adapters receive a Runtime reference to access services (`SetRuntime(rt any)`, type-asserted to `*runtime.Runtime` to avoid an import cycle)
- `BaseAdapter` provides the shared TCP lifecycle: accept loop with backoff, graceful drain, forced close, and listener-readiness signalling
- Lifecycle: `SetRuntime() -> Serve() -> Stop()`
- Multiple adapters can share the same runtime
- Thread-safe, supports graceful shutdown

**4. Auth** (`pkg/auth/`)
- Shared authentication and identity primitives used across protocols
- Sub-packages:
  - `kerberos/`: Kerberos `Provider` — keytab and krb5.conf state with hot-reload
  - `sid/`: Windows SID types, encoding, and cross-protocol mapping

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

**6. Block storage** (`pkg/block/`)
- The data path is cut: no share holds a content store, and every operation that reads or writes file content answers `block.ErrNoContentStore` (see [Data path](#data-path)).
- `engine/`: the engine (RFC 8) over `journal/`, the journal (RFC 1). After the cutover the adapters reach it through `vfs.Service`.
- `remote/`: block store interface and implementations (`s3/` production, `memory/` testing). Each share binds one block store config; `shares.Service` acquires its remote store on AddShare and releases it on RemoveShare, ref-counted per config across the shares that use it.
- `gc/`: mark-sweep over the remote stores and the metadata manifests.

**7. Metadata Store** (`pkg/metadata/store.go`)
- **Simple CRUD interface** for file/directory metadata
- Stores file structure, attributes, permissions
- Implementations:
  - `pkg/metadata/store/badger/`: BadgerDB (embedded, path-based handles), the
    only backend — on disk for single-node deployments, or fully in memory
    (`in_memory: true`) for tests
- File handles are opaque identifiers (implementation-specific format)

## Data path

No share holds a content store until the adapters run on `vfs.Service`. Every
operation that reads or writes file content answers `block.ErrNoContentStore`
(`pkg/block/errors.go`), carried to the adapters as `common.ErrNoContentStore`
in the I/O row, so each protocol answers its generic I/O status. Metadata
operations run against the metadata service as before; a size change is a
metadata update only, and an operation that touches no content (a zero-count
WRITE, COMMIT or FLUSH of an empty file) succeeds. Snapshot create and restore
answer the same sentinel, since a snapshot captures content.

Each share keeps a state directory beneath `blockstore.journal.path`
(`<path>/shares/<share-name>/`) holding its gc-state, snapshots and migration
journal, and binds one remote store, ref-counted per block store config across
the shares that use it.

The engine (`pkg/block/engine`, [RFC 8](rfcs/rfc-8-engine.md)) and the journal
(`pkg/block/journal`, [RFC 1](rfcs/rfc-1-journal.md)) are built and tested on
their own, behind the `engine_conformance` and `journal_conformance` tags.

## Garbage Collection (mark-sweep)

The block-store GC is a fail-closed mark-sweep over the union of every live
`FileChunk.ContentHash` across all shares pointing at the same remote.

### Algorithm

1. **Mark phase.** Stream every `FileChunk`'s `ContentHash` via the
   `MetadataStore.EnumerateFileChunks(ctx, fn)` cursor. The cursor
   is implemented natively by the Badger store and
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
dead bytes forever. Compaction (`gc.CompactBlocks`, #1487) closes that gap.
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

- **Periodic** via the background scheduler, on by default. `dfs start`
  runs GC every `gc.auto_interval` (default 15m; values in (0, 1m) are
  refused) unless `gc.auto_enabled` is false. See
  [Garbage Collection](../guide/garbage-collection.md#automatic-gc-default).
- **On-demand** via `dfsctl store block gc <share> [--dry-run]`;
  `--dry-run` skips DELETEs and prints up to `gc.dry_run_sample_size`
  candidate keys (default 1000). The mark-sweep is global across every
  share that targets the same remote, and every run scans every remote.
  The `<share>` argument only selects where the run summary
  (`last-run.json`) is written; it does not choose the remotes or scope
  the live set to one share.
- **Observability** via structured slog INFO at start/end with `run_id`,
  `hashes_marked`, `objects_swept`, `bytes_freed`, `duration_ms`, and
  `error_count`. An on-demand run also persists its summary at
  `<localStore>/gc-state/last-run.json`; inspect it with
  `dfsctl store block gc-status <share>`. A scheduled run persists none.

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

See `docs/guide/configuration.md` for every `gc.*` and `syncer.*` knob, and
`docs/guide/cli.md` for the `dfsctl store block gc` reference.

## Share Snapshots

Share snapshots are point-in-time, reference-based protection for a
share's content. The subsystem produces three artifacts per snapshot
on local disk and one row in the control-plane database; it does not
copy any block data. See [SNAPSHOTS.md](../guide/snapshots.md) for the
operator-facing runbook; this section describes the architectural
layout and the orchestration flows.

### Subsystem layout

| Location | Role |
| --- | --- |
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
              ─→ DrainAllUploads (always — the dump must carry every block locator)
              ─→ Dump metadata to metadata.dump
              ─→ Build hash manifest from CAS
              ─→ Verify gate: DrainAllUploads + VerifyRemoteDurability
                 (both skipped if NoVerify, concurrency = 16)
              ─→ Update row state=ready (or failed) + remote_durable flag
```

`Runtime.CreateSnapshot` returns the new snapshot ID immediately and
runs the orchestration in a background goroutine. The REST handler
returns `202 Accepted` with a `Location` header pointing at the
record; callers poll `GET /snapshots/{id}` until `state != "creating"`.
The CLI's `WaitForSnapshot` does that polling on the operator's
behalf.

`NoVerify=true` (CLI `--no-verify`) skips the verify gate — its upload
drain and the HEAD-probe phase. It does not skip the drain that runs
before the metadata dump, so a block store that cannot be reached fails
snapshot creation either way. The snapshot still completes with
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
| --- | --- | --- |
| `POST` | `/` | 202 Accepted + `Location` header |
| `GET` | `/` | 200 OK + JSON array (empty: `[]`, not `null`) |
| `GET` | `/{id}` | 200 OK + full record |
| `DELETE` | `/{id}` | 204 No Content |
| `POST` | `/{id}/restore` | 200 OK + `{snapshot_id, safety_snapshot_id, share}` |

The single `mapSnapshotError` helper handles the 14 typed sentinels
that can cross the boundary (12 snapshot sentinels + share-not-found
- nil-guard). The mapping table lives in the handler file as the
sole source of truth; future sentinels add a single case.

The Restore handler wraps `r.Context()` with
`context.WithTimeout(ctx, cfg.Snapshot.restore_http_timeout)`
(default 30 minutes) to bound runaway restores. The apiclient
mirrors the timeout on the client's `http.Client` for the restore
call only (`WithRestoreTimeout`).

For the full operator runbook see
[SNAPSHOTS.md](../guide/snapshots.md).

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

// RuntimeSetter - adapters that need runtime access implement this.
// The parameter is `any` on purpose: pkg/adapter cannot import
// pkg/controlplane/runtime (the runtime imports the adapter contract), so
// implementations type-assert to *runtime.Runtime and panic on a mismatch.
type RuntimeSetter interface {
    SetRuntime(rt any)
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

- **No content store**: `common.ErrNoContentStore` is the answer every
  data operation gives while the data path is cut: `block.ErrNoContentStore`
  in the I/O row, so each protocol's `StatusForErr` maps it to its own I/O
  status.
- **Metadata error translation**: per-package `StatusFor` switches in the
  adapter types packages (`internal/adapter/nfs/types`,
  `internal/adapter/nfs/v4/types`, `internal/adapter/smb/types`), each with
  its enum-walk test; `StatusForErr` is the error-level wrapper. SMB lock-
  operation context uses the parallel `StatusForLock`/`StatusForLockErr`
  functions in the same package (e.g., `ErrLocked` →
  `STATUS_LOCK_NOT_GRANTED` in lock context vs. `STATUS_FILE_LOCK_CONFLICT`
  in general I/O context). Adding a new `metadata.ErrorCode` means adding a
  switch arm plus an expectation row in each relevant package — the
  enum-walk test fails loudly if either is missing.

See CONTRIBUTING.md "Adding a new metadata.ErrorCode" for the recipe and
NFS.md / SMB.md "Error mapping" for protocol-specific notes.

### Object ownership: a client-supplied identifier is not authorization

Every protocol here lets a client name a server-side object by an identifier the
server issued earlier — an SMB `TreeID` or `FileId`, an SMB `AsyncId`, an NFSv4
stateid. **An operation must verify that the named object belongs to the
requesting session or client before acting on it, and especially before adopting
its identity.** Resolving the identifier proves the object exists; it says
nothing about who is entitled to it.

The checks live at choke points rather than in each handler, so an operation
added later inherits them:

- **SMB tree-scoped commands** — `prepareDispatch`'s `NeedsTree` branch
  (`internal/adapter/smb/response.go`) rejects a request whose
  `tree.SessionID` differs from the header's. It is the only dispatch gate,
  so TREE_DISCONNECT, CREATE and every tree-scoped command are covered once.
- **SMB file handles** — `primeAuthContextFromOpenFile`
  (`internal/adapter/smb/handlers/auth_helper.go`) refuses with
  `STATUS_FILE_CLOSED` unless `openFileBelongsToRequest` matches both the tree
  and the session. This matters more than a plain existence check because the
  function then *prefills the auth context from the located handle*: without
  the guard, a request naming another session's `FileId` would execute as that
  handle's user.
- **SMB parked requests** — `pendingRegistry.unregisterByAsyncIDOn`
  (`internal/adapter/smb/pending/pending_registry.go`) scopes an `AsyncId`
  lookup to the connection that parked it, as the `MessageID` lookups already
  did, so a CANCEL cannot retire another connection's request.
- **NFSv4 stateids** — `ValidateStateid` takes the caller's client ID and
  compares it through `checkStateidOwner`
  (`internal/adapter/nfs/v4/state/stateid.go`) for all three
  stateid families, so the I/O operations are covered at one point. The
  state-changing operations do not go through `ValidateStateid`, and reach the
  same comparison two ways: `CloseFile`, `ConfirmOpen`, `ConfirmOpenV41`,
  `DowngradeOpen`, `ReturnDelegation` and the three TEST_STATEID probes call
  `checkStateidOwner` themselves, while the byte-range lock paths
  (`LockNew`, `LockExisting`, `UnlockFile`) go through
  `revalidateLockStateLocked`, which additionally compares the lock state and
  lock owner **by pointer identity** — a stateid `other` can be reissued after
  the original state is freed, so re-finding an entry under that key is not
  proof it is the same entry.

Two exceptions are deliberate, and the rule is not true without them:

1. **NFSv4.0 carries no trusted client identity** on the operations above, so
   the caller's client ID is zero there and `checkStateidOwner` skips the
   comparison by design. The binding is real for v4.1 and later only; on v4.0
   the unguessability of the stateid is all that stands behind it.
2. **Per-handler `GetTree(ctx.TreeID)` lookups stay existence-only**, because
   the dispatcher already bound that tree to the session before the handler
   ran. A `ponytail:` comment at those sites names the ceiling.

Unguessability is not the guarantee. A stateid's `other` and an SMB `FileId`
each carry 64 bits from `crypto/rand`, which makes them infeasible to forge but
does nothing once one has been observed — so these checks are defence in depth
layered under the identifier's entropy, not a substitute for it.

**Extending this:** an operation that resolves a client-supplied identifier
routes through the existing choke point instead of adding a comparison of its
own, and ships with a test that fails when the guard is removed. A guard that
has never refused anything in a test is unverified.

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
./dfsctl store metadata add --name fast-meta --in-memory
./dfsctl store metadata add --name persistent-meta --type badger \
  --config '{"path":"/data/metadata"}'

# Create block stores (shared across shares)
./dfsctl store block add --name mem-blocks --type memory
./dfsctl store block add --name s3-remote --type s3 \
  --config '{"region":"us-east-1","bucket":"my-bucket"}'

# Create shares referencing stores by name (each gets its own BlockStore + journal)
./dfsctl share create --name /temp --metadata fast-meta --block-store mem-blocks
./dfsctl share create --name /archive --metadata persistent-meta \
  --block-store s3-remote
```

### Benefits

- **Per-share isolation**: Each share gets its own BlockStore and its own journal directory
- **Resource Efficiency**: Block stores are shared (ref counted) when multiple shares reference the same config
- **Flexible Topologies**: Different shares can target different block stores
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
metaSvc.RegisterStoreForShare("/export", inMemoryBadgerStore)
metaSvc.RegisterStoreForShare("/archive", badgerStore)

// High-level operations (with business logic)
file, err := metaSvc.CreateFile(authCtx, parentHandle, "test.txt", fileAttr)
entries, err := metaSvc.ReadDir(ctx, dirHandle)

// Byte-range locking (SMB/NLM)
lock, err := metaSvc.AcquireLock(ctx, shareName, handle, offset, length, exclusive)
```

### Write Coordination Pattern

WRITE validates its target with `metaSvc.PrepareWrite` (permissions, type,
read-only share), which modifies no metadata, so a refused write answers with
its own status. A zero-count WRITE then succeeds; any other answers
`common.ErrNoContentStore` until the adapters run on `vfs.Service`.

## Built-In and Custom Backends

### Using Built-In Backends

No custom code required - configure via CLI:

```bash
# Create stores
./dfsctl store metadata add --name default-meta --type badger \
  --db-path /data/metadata   # or --in-memory for tests

# Create share referencing the stores
./dfsctl store block add --name default-blocks --type memory
./dfsctl share create --name /export --metadata default-meta --block-store default-blocks
```

### Implementing Custom Store Backends

See [docs/IMPLEMENTING_STORES.md](implementing-stores.md) for detailed implementation guides for:
- **Block Store**: Implement `pkg/block/remote.RemoteStore` (`pkg/block/remote/remote.go`) interface
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
│   │   ├── adapter.go            # Adapter, OplockBreaker interfaces
│   │   ├── base.go               # BaseAdapter shared TCP lifecycle
│   │   ├── healthcheck.go        # BaseAdapter.Healthcheck
│   │   ├── identity.go           # BuildIdentityResolver, ExtractRealm
│   │   ├── sidecar/              # Sidecar-service lifecycle group
│   │   ├── nfs/                  # NFS adapter: dispatch.go routes RPC procedures
│   │   └── smb/                  # SMB adapter implementation
│   │
│   ├── auth/                     # Shared authentication primitives
│   │   ├── kerberos/             # Kerberos Provider
│   │   │   ├── provider.go       # Provider (keytab/krb5.conf state)
│   │   │   ├── keytab.go         # Keytab hot-reload manager
│   │   │   └── doc.go            # Package doc
│   │   └── sid/                  # Windows SID types and mapping
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
│   │   ├── lock_exports.go       # LockManager surface for byte-range locks
│   │   ├── acl/                  # Windows ACL model and ACE evaluation
│   │   ├── lock/                 # Lock manager, break/grace machinery
│   │   ├── storetest/            # Conformance test suite for store implementations
│   │   └── store/                # Store implementations
│   │       ├── badger/           # BadgerDB (on disk or in memory)
│   │       ├── basestore/        # Store-agnostic helpers
│   │       └── internal/         # Caches, retry
│   │
│   ├── block/                    # Per-share block storage
│   │   ├── blockstore.go         # BlockStore interface
│   │   ├── types.go              # Block, BlockState types
│   │   ├── errors.go             # Block store error types
│   │   ├── chunker/              # FastCDC content-defined chunker
│   │   │                         # min=1 MiB / avg=4 MiB / max=16 MiB, lvl 2;
│   │   │                         # BLAKE3 hashing; consumed by the carve pass
│   │   ├── carver/               # Carve pass: chunk a file into blocks
│   │   ├── blockcodec/           # On-disk block payload encoding
│   │   ├── middleware/           # The remote-store decorators
│   │   │   ├── compression/      # Optional per-chunk compression
│   │   │   └── encryption/       # Optional per-chunk encryption
│   │   ├── engine/               # The engine (RFC 8), not yet wired to the adapters
│   │   ├── journal/              # The journal (RFC 1), one per device
│   │   │   ├── localstore.go     # LocalStore interface (*Store implements it)
│   │   │   └── journaltest/      # Throwaway real store for outside tests
│   │   ├── syncer/               # Local -> remote sync
│   │   ├── blockstoretest/       # Conformance suites for block store impls
│   │   └── remote/               # Block store interface
│   │       ├── s3/               # S3-backed block store
│   │       └── memory/           # In-memory block store (testing)
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
│   ├── identity/                 # Identity resolution (providers, resolver, cache)
│   ├── discovery/                # Service discovery (mDNS/DNS-SD)
│   ├── health/                   # Health/readiness aggregation
│   ├── metrics/                  # Prometheus metrics
│   ├── netutil/                  # Network helpers
│   ├── schedule/                 # Scheduled task runner
│   ├── snapshot/                 # Snapshot/backup support
│   │
│   └── config/                   # Configuration parsing
│       ├── config.go             # Main config struct
│       ├── blockstore.go         # Block store config
│       ├── metadata.go           # Metadata store config
│       ├── defaults.go           # Defaults + env binding
│       └── ...                   # Per-section config (logging, gc, snapshot, ...)
│
├── internal/                     # Private implementation details
│   ├── adapter/common/           # Shared NFS/SMB adapter helpers:
│   │   └── content.go            # ErrNoContentStore, the data path's answer
│   ├── adapter/nfs/              # NFS protocol internals
│   │   ├── rpc/                  # RPC layer (call/reply handling)
│   │   │   └── gss/              # RPCSEC_GSS framework
│   │   ├── xdr/core/             # Generic XDR codec
│   │   ├── auth/                 # Auth context, AUTH_UNIX, share permission
│   │   ├── middleware/           # Per-request auth extraction
│   │   ├── types/                # NFS constants and types
│   │   ├── mount/handlers/       # Mount protocol procedures
│   │   ├── nlm/, nsm/, portmap/  # Lock/status/portmapper sidecar services
│   │   ├── v3/handlers/          # NFSv3 procedures (READ, WRITE, etc.)
│   │   └── v4/handlers/          # NFSv4.0 and v4.1 procedures
│   ├── adapter/smb/              # SMB protocol internals
│   │   ├── auth/                 # NTLM/SPNEGO authentication
│   │   ├── handlers/             # SMB2 command handlers
│   │   ├── session/, lease/      # Session + oplock/lease state
│   │   ├── signing/, encryption/, kdf/   # Crypto primitives
│   │   ├── framing.go            # NetBIOS framing
│   │   └── dispatch.go           # Command dispatch
│   ├── controlplane/api/         # API implementation
│   │   ├── handlers/             # HTTP handlers with centralized error mapping
│   │   └── middleware/           # Auth middleware
│   ├── auth/                     # Kerberos/Netlogon service internals
│   ├── cli/                      # CLI output formatting
│   ├── logger/                   # Logging utilities
│   └── ...                       # tlsconfig/, pathutil/, bytesize/, sysinfo/
│
├── docs/                         # Documentation
│   ├── internals/                # This file, protocol and store internals
│   ├── guide/                    # User guides (configuration, NFS, SMB, CLI)
│   └── ...
│
└── test/                         # Test suites
    ├── integration/              # Integration tests (S3, BadgerDB)
    ├── e2e/                      # End-to-end tests (real NFS mounts)
    ├── conformance/              # SMB/NFS conformance harnesses
    ├── posix/, edge/, crash/     # Behavioural and fault-injection suites
    └── spec-citations/           # Verifies spec-section citations in code
```

## Multi-Node Metadata

Badger is embedded in one `dfs` process, so a metadata store serves exactly one
server. A distributed transactional key-value store — TiKV is the preferred
one — is the planned backend for multi-node deployments; it is not implemented
yet.

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

## Migration & Block-Layout Routing

DittoFS has had three block layouts (see the migration guide's table). Neither
transition into the current one is performed by this build any more: both
conversions shipped, and both have been removed. A share still on an older
layout must be staged through a release that still carries the conversion, or
re-ingested.

### Standalone CAS (v0.16-v0.21) -> packed blocks: removed

The current layout packs chunks into `blocks/<id>` container objects. A share
carrying leftover standalone-CAS state — remote `cas/` objects, or chunk
locators that still point at standalone objects — is no longer converted. The
conversion ran at engine start through v0.31 and is gone; nothing
replaces it, and there is no boot-time scan for the state it handled.

The read path refuses that state rather than guessing, one chunk at a time:

- A synced locator with an empty `BlockID` fails closed with `ErrChunkNotFound`
  (`engine.resolveAndReadChunk`), because an empty block id would otherwise
  resolve to a bogus object key and could surface as zeros.
- A `FileChunk` with a zero `Hash` predates content addressing entirely and is
  refused for the same reason (`engine.dispatchRemoteFetch`).

Both log what the operator has to do. **There is no boot signal**: such a share
starts normally and fails reads as they arrive, which is why staging the upgrade
matters more than it used to.

Two consequences worth knowing:

- Pre-flip `cas/` objects are never purged. The GC sweep reclaims only what a
  share resolves to a block locator, and fails toward leaking rather than
  deleting an object it cannot attribute, so leftover `cas/` objects are billed
  until removed by hand.
- The hash-keyed CAS accessors are gone entirely. They were removed along with
  the `block.Store` interface, on the backends and the decorators alike, so
  nothing in the tree can read a `cas/` object any more — not even by
  type-asserting past `remote.RemoteStore`.

### Pre-v0.16 `.blk` -> CAS: migrate with dittofs <= v0.21

The offline `.blk`->CAS tool (`migrate-to-cas`) shipped through v0.21 and has
been removed. The journal format stamp (`cmd/dfs/commands/start.go`'s
`handleFormatMismatch`) refuses a directory a newer release wrote and exits 78
(`EX_CONFIG`); the pre-journal blobs/+logs/ guard was deleted with the
pre-journal local tier — no production stores exist in field, so opening such a
directory as an empty journal is accepted. Unlike the standalone-CAS case
above, this one is a read-time answer, not a boot refusal.

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
