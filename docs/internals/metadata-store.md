# The Metadata Store

This document explains how DittoFS's **metadata store** is designed and how each of
its four backends realizes the same domain model. It is the design/implementation
companion to [Implementing Custom Stores](./implementing-stores.md), which documents
the *contract* a new backend must satisfy. Read that one to build a backend; read this
one to understand what the existing backends actually store and why they diverge.

Scope: the metadata store owns the filesystem namespace, per-file attributes, and the
pointers into the block layer. It does **not** own file *data* — that lives in the
per-share block store (see [Architecture](./architecture.md) and
[Implementing Custom Stores](./implementing-stores.md)). The two coordinate on WRITE,
in the order fixed by the architecture invariants.

## Table of Contents

- [The entity model](#the-entity-model)
- [The store interface](#the-store-interface)
- [How each backend realizes the model](#how-each-backend-realizes-the-model)
  - [Badger (default, embedded)](#badger-default-embedded)
  - [Postgres and SQLite (relational)](#postgres-and-sqlite-relational)
  - [Memory (ephemeral)](#memory-ephemeral)
- [Same entity, three encodings](#same-entity-three-encodings)
- [Durability model](#durability-model)
- [Known simplification work](#known-simplification-work)

---

## The entity model

The domain types live in `pkg/metadata/` (and the `pkg/block` value types they embed).
Each entity has a **persistence class** that describes how it lands in a store:

- **Persisted** — has its own identity/key (a Badger key prefix, a SQL row/table).
- **Embedded** — rides inside another entity's serialized form; no identity of its own.
- **Derived** — synthesized on read, never stored.
- **Transient** — in-memory only, never persisted.

### The aggregate root: `File` + `FileAttr`

`File` (`pkg/metadata/file_types.go`) is the aggregate root: `{ID uuid.UUID, ShareName
string, Path string, FileAttr}`. `FileAttr` is the embedded attribute bag and makes up
the bulk of it — `Type, Mode, UID/GID, Nlink, Size, Atime/Mtime/Ctime/CreationTime,
PayloadID, LinkTarget, Rdev, Hidden, ACL, EAs, IdempotencyToken, Blocks, ObjectID`, and
the recycle fields `DeletedAt/OriginalPath/DeletedBy`.

`File` is **persisted**; `FileAttr` is **embedded** in it. Two fields are special:

- `Path` is **derived** — zeroed before the record is written and rebuilt on read by
  walking the parent/child edges (`derivePath`, O(1) via the reverse-edge index; #1166).
- `Nlink` is **duplicated** in Badger (see the link-count entity below); the relational
  backends keep it as a single `nlink` column.

Files are addressed by an opaque `FileHandle` encoded as `shareName:uuid`
(`EncodeFileHandle`/`DecodeFileHandle`, `pkg/metadata/types.go`). The share-name prefix
is what lets the Runtime route a handle to the right per-share block store.

### Content manifest: `Blocks []ChunkRef`, `ObjectID`, `PayloadID`

A regular file's data is a list of content-addressed chunks:

- **`ChunkRef`** (`pkg/block/types.go`) — `{Hash ContentHash; Offset uint64; Size
  uint32}`. The `Blocks []ChunkRef` slice is the per-file manifest. `ContentHash` is a
  32-byte BLAKE3 digest that custom-marshals to `"blake3:{hex}"` in JSON. This is the
  **heaviest embedded field** on a Badger inode.
- **`ObjectID`** (`pkg/block/types.go`) — a BLAKE3 Merkle root over the sorted chunk
  hashes; the file-level dedup key. Embedded in the inode **and** carried in a reverse
  index for `FindByObjectID`. The all-zero value means "never quiesced".
- **`PayloadID`** (`pkg/metadata/file_types.go`) — the content identity `share/<uuid>`;
  embedded scalar plus its own reverse index.

### ACLs and extended attributes

- **`ACL`** (`pkg/metadata/acl/`) — NFSv4/Windows ACL, an `omitempty` pointer field on
  `FileAttr`. `nil` means classic Unix mode bits. See
  [ACL Design](./acl-design.md) for the ACL model itself.
- **EAs / xattrs** — the `EAs map[string][]byte` field on `FileAttr`. There is a single
  logical xattr namespace resolved in `pkg/metadata/xattr.go` over two physical backings
  (inline `FileAttr.EAs` and named-stream child entities), with stream-wins precedence.
  The inline backing rides the inode blob; setting an inline xattr rewrites the whole
  `FileAttr`.

### Namespace edges and link count

The directory tree is stored as edges rather than nested structures:

- **parent edge** — child → parent (`GetParent`/`SetParent`).
- **child edge** — (parent, name) → child (`GetChild`/`SetChild`/`ListChildren`).
- **child-name reverse edge** — (parent, child) → name, so `derivePath` is O(1).
- **link count** — the hard-link count (`GetLinkCount`/`SetLinkCount`), used for orphan
  detection (`nlink == 0`).

### Block-layer records

These entities are how the metadata store participates in block dedup, sync, and GC:

- **`BlockRecord`** (`pkg/block/block_record.go`) — `{BlockID, BlockHash, Length,
  LiveChunkCount, SyncState}` for a packed block; consumed by the engine's GC. Persisted.
- **Synced marker** — `hash → (synced-at nanos + ChunkLocator)`; the fact "this chunk is
  durable on the remote". `ChunkLocator` (`pkg/block/locator.go`) is `{BlockID,
  WireOffset, WireLength}`. Persisted; consumed by engine sync + GC.
- **`LocalChunkLocation`** (`pkg/block/block_record.go`) — `{LogBlobID, RawOffset,
  RawLength}`, the local log-blob position of a chunk. Persisted, but **vestigial** once
  the journal redesign (#1692) owns local reads (see [Known simplification
  work](#known-simplification-work)).
- **Rollup offset** — a per-payload `uint64` fence used by the legacy local log-blob
  store. Persisted scalar; **vestigial** alongside `LocalChunkLocation`.
- **`FileChunk`** (`pkg/block/types.go`) — a rich per-chunk DTO
  (`ID, Hash, DataSize, LocalPath, BlockStoreKey, RefCount, LastAccess, State, …`). In
  Badger this is **derived** — a projection over the inode's `Blocks[]` plus the block
  records and synced markers — with no key of its own. The relational backends store it
  as a real `file_blocks` row. This is the single biggest badger↔SQL modelling divergence.

### Shares, config, and filesystem meta

- **`Share`** (`pkg/metadata/types.go`) + its root-dir `FileHandle` — persisted together
  (`AddShare`/`UpdateShare`/`GetRootHandle`).
- **`MetadataServerConfig`** — server-wide settings singleton.
- **`FilesystemCapabilities`** (static limits/features) — persisted singleton. The
  dynamic half, `FilesystemStatistics` (usage), is **derived** — recomputed per statfs;
  per-user/per-group quota counters are rebuilt from the inode set at startup rather than
  persisted.

### Locks, NSM, durable handles, recycle — at a glance

These are persisted but out of scope here (protocol-lock semantics are documented with
their protocols):

- **NLM/lease locks** — persisted lock records + indexes.
- **NSM client registrations** — persisted client records.
- **SMB durable handles** — persisted handle records + indexes (SMB path only, not the
  NFS hot path).
- **Recycle/trash** — not a separate entity; `DeletedAt/OriginalPath/DeletedBy` are
  embedded fields on `FileAttr`, and trash listing scans for non-nil `DeletedAt`.
- **`ShareSession`** — **transient**; a mount-tracking value used only by the memory
  store's in-RAM session map. Never persisted by any durable backend.

---

## The store interface

Every backend implements `metadata.Store` (`pkg/metadata/store.go`). `Store` embeds the
`Files` interface (the CRUD surface) and adds transaction control. The key operation
groups:

| Group | Operations | Notes |
|---|---|---|
| File CRUD | `GetFile`, `PutFile`, `DeleteFile` | No permission checks or validation at this tier — the caller (metadata service) owns those. |
| Namespace edges | `GetChild`, `SetChild`, `DeleteChild`, `ListChildren` (cursor-paginated), `GetParent`, `SetParent` | Directory structure as edges. |
| Link count | `GetLinkCount`, `SetLinkCount` | `nlink`; orphan detection. |
| Xattr | `GetXattr`, `SetXattr`, `RemoveXattr`, `ListXattr` | Single namespace over inline + stream backings (`xattr.go`). |
| Handles | `GenerateHandle`, encode/decode | Opaque, share-scoped. |
| Blocks / manifest | manifest read/write, `EnumerateFileChunks` (GC cursor), block-record + synced-marker commit | Coordinates with the block engine. |
| Transactions | `WithTransaction` (fully durable), `WithTransactionRelaxed` (durability deferred) | See [Durability model](#durability-model). |

### Invariants the store must preserve

These are the metadata-relevant slice of the repo's [architecture
invariants](../../CLAUDE.md) — do not restate the block-store rules here:

- **File handles are opaque.** Generated by the store, they encode share identity so the
  Runtime can route. Protocol handlers never parse them, and they must stay stable across
  restarts for persistent backends.
- **Every operation carries an `*metadata.AuthContext`.** It threads RPC → handler →
  store. The bare `Files` methods do **no** permission checking; that is enforced one
  layer up in `pkg/metadata` (the service layer), which is why `GetFile` documents "NO
  permission checking — caller is responsible".
- **WRITE coordinates metadata + block store** in the fixed order (metadata write-path
  permission/size/mtime update → resolve the per-share block store → block write → return
  updated attrs). The store's job is the metadata half.
- **Error codes** are the `metadata.ExportError` values (`ErrNotFound`, `ErrNotDirectory`,
  `ErrAccess`, `ErrExist`, `ErrNotEmpty`, …).

---

## How each backend realizes the model

Reference implementations: `pkg/metadata/store/{badger,postgres,sqlite,memory}/`.
Conformance suite (mandatory for any backend): `pkg/metadata/storetest/`.

### Badger (default, embedded)

The default backend is an embedded BadgerDB LSM store. Everything is a key with a short
prefix; the inode is a **single JSON blob** and the structural relationships are separate
edge/index keys.

| Entity | Prefix | Key → Value |
|---|---|---|
| File inode | `f:` | `f:<uuid>` → whole `File`/`FileAttr` JSON (incl. ACL, EAs, `Blocks[]`, `ObjectID`, recycle fields) |
| Parent edge | `p:` | `p:<childUUID>` → parentUUID |
| Child edge | `c:` | `c:<parentUUID>:<name>` → childUUID |
| Child-name reverse edge | `cn:` | `cn:<parentUUID>:<childUUID>` → name |
| Link count | `l:` | `l:<uuid>` → uint32 (authoritative; overrides the blob's `Nlink` on read) |
| ObjectID index | `obj:` | `obj:<hex>` → fileUUID (written only when `ObjectID` changes) |
| PayloadID index | `pl:` | `pl:<payloadID>` → fileUUID |
| Block record | `br:` | `br:<blockID>` → `BlockRecord` JSON |
| Local chunk index | `li:` | `li:<hex(hash)>` → `LocalChunkLocation` JSON *(vestigial, see #1692/#1715)* |
| Rollup offset | `ro:` | `ro:<payloadID>` → uint64 *(vestigial)* |
| Synced marker | `synced:` | `synced:<hash>` → nanos + `ChunkLocator` |
| Share | `s:` | `s:<name>` → `Share` + RootHandle JSON |
| Server config / FS caps / FS meta | `cfg:` / `cap:` / `fsmeta:` | singletons / per-share |
| Locks / NSM / durable handles | `lock:` `lkfile:` … / `nsm:…` / `dh:…` | out of scope here |

Key design consequences:

- **The inode is one blob.** Every mutation — including a hot CLOSE flush that only bumps
  `Size`/`Mtime`/`Ctime` — re-serializes the entire `File` JSON, `Blocks[]` and any
  ACL/EAs included. `omitempty` keeps small-file inodes compact, but a large manifest or
  ACL bloats every rewrite. This is the write-amplification the #1715 `fb:` split targets.
- **Structure is edges, content is embedded.** parent/child/nlink/objectid/payload are
  separate keys; FileAttr/ACL/EAs/`Blocks[]`/recycle state are embedded in the `f:` blob.
- **`FileChunk` is a projection**, not a stored row — synthesized from `Blocks[]` + `br:`
  + `synced:`.
- **One confirmed-dead prefix**: `d:` (device numbers). Device major/minor actually live
  in `FileAttr.Rdev` inside the `f:` blob; the `d:` prefix has no reader or writer.

### Postgres and SQLite (relational)

Both relational backends are **hand-written raw SQL** (pgx for Postgres, `database/sql`
for SQLite) — **not GORM** (the two "gorm" comments only note that the control-plane GORM
layer shares the driver). The GORM footguns that bite the control plane do **not** apply
here. They normalize into tables what Badger embeds:

| Table | Holds | Badger equivalent |
|---|---|---|
| `inodes` (Postgres historically `files`) | one row per file: id, share_name, file_type, mode, uid, gid, size, **nlink** (single column), times, content_id, link_target, device_major/minor, hidden, **acl**, **eas**, **object_id**, recycle cols | `f:` blob **minus** `Blocks[]` and the `l:` link-count key |
| `parent_child_map` | (parent_id, child_name) → child_id, with a reverse child_id index | `c:` / `p:` / `cn:` edges |
| `file_block_refs` | the per-file manifest, one row per chunk, PK `(file_id, offset)` | `Blocks []ChunkRef` (a **join table** instead of an embedded slice) |
| `file_blocks` | the real `FileChunk` row (hash, cache_path, block_store_key, ref_count, state, …) with partial indexes for syncer/GC/evict | *derived* in Badger |
| `block_records` / `local_chunk_index` / `rollup_offsets` | SQL twins of `br:` / `li:` / `ro:` | same verdicts (`local_chunk_index`, `rollup_offsets` vestigial) |
| `synced_hashes` | remote-durable marker, PK `hash` + locator columns | `synced:` |
| `shares`, `server_config`, `filesystem_capabilities`/`filesystem_meta`, `server_epoch` | singletons / control-plane | `s:` / `cfg:` / `cap:` / `fsmeta:` |
| `locks`, `nsm_client_registrations`, `durable_handles` | out of scope here | `lock:` / `nsm:` / `dh:` |

Where SQL normalizes what Badger embeds:

- **`file_block_refs` join table** replaces the embedded `Blocks[]`. It was made a side
  table *deliberately* to dodge TOAST/blob write amplification — the exact problem the
  Badger `f:` blob still has. (Note: because the manifest is rewritten wholesale on every
  `PutFile` today, the SQL side has its own amplification on attr-only changes; see
  [Known simplification work](#known-simplification-work).)
- **`nlink` is a single column.** Postgres once had a separate `link_counts` table and
  *dropped* it, making the column authoritative. Badger is the only backend still carrying
  the count in two places (`l:` key + blob field).

Postgres vs SQLite differ in operational shape, not in the model:

| Aspect | Postgres | SQLite |
|---|---|---|
| Concurrency | connection pool | **single writer** (`MaxOpenConns=1`) — its real bottleneck |
| Per-commit fsync | `synchronous_commit=on`, relaxable to `off` for namespace writes | **off by default** (`journal_mode=WAL`, `synchronous=NORMAL`) — fsync only at checkpoint |
| Migrations | many incremental | consolidated (no legacy installs) |

### Memory (ephemeral)

`pkg/metadata/store/memory/` is an in-RAM implementation backed by Go maps under a mutex.
It is used for ephemeral shares and tests, passes the same `storetest` conformance suite,
and is the sole user of the transient `ShareSession`. Nothing is durable; there is no
fsync path.

---

## Same entity, three encodings

The single most useful cross-backend view — how the same domain concepts land in each
backend:

| Domain concept | Badger | Postgres / SQLite | Memory |
|---|---|---|---|
| **File + attributes** | one `f:<uuid>` JSON blob | one `inodes` row | struct in a map |
| **Content manifest** (`Blocks []ChunkRef`) | **embedded** JSON array in the `f:` blob | **normalized** `file_block_refs` join table (row per chunk) | slice on the struct |
| **`FileChunk`** (per-chunk lifecycle) | **derived** projection (`Blocks[]` ⨝ `br:` ⨝ `synced:`) — no key | real `file_blocks` row | derived / map |
| **`nlink`** | **duplicated**: `l:<uuid>` key (authoritative) **and** blob field | single `nlink` column | struct field |
| **`ObjectID`** | embedded in blob + `obj:<hex>` reverse index | `object_id` column + partial-unique index | struct field + map index |
| **Namespace edge** | `c:`/`p:`/`cn:` keys | `parent_child_map` rows | nested maps |
| **`Path`** | **derived** (zeroed on write, `derivePath` on read) | derived (no path column; #1166) | derived |
| **ACL / EAs** | embedded nested JSON in the blob | `acl` / `eas` columns (in-row) | struct fields |
| **Synced marker** | `synced:<hash>` → nanos + locator | `synced_hashes` row | map |

Reading the table: Badger optimizes for a single-key inode read (everything in one blob)
at the cost of rewriting that blob on every mutation; the relational backends normalize
the heavy/mutable parts (`Blocks[]`, `FileChunk`) into side tables so an attribute change
touches fewer bytes. The two places SQL *deliberately* normalized away — `Blocks[]` and
`nlink` — are exactly the two Badger simplification candidates.

---

## Durability model

Commit boundaries are **backend-agnostic**: the metadata service (`pkg/metadata/io.go`,
`service.go`) decides how many transactions a file op spans and whether each is fully
durable or relaxed. What differs per backend is what a commit *costs*.

Two write-path behaviors set the shape:

- **`deferredCommit` (default true).** Per-WRITE RPCs accumulate in a RAM
  `pendingWrites` buffer and are flushed by **one** durable transaction on NFS
  COMMIT/close. So a steady stream of 4 KiB writes touches the store zero times until the
  flush. READ does not persist atime (there is no atime-on-read write).
- **Relaxed vs strict transactions.** `WithTransaction` is fully durable;
  `WithTransactionRelaxed` defers the fsync. Namespace/attribute writes generally run
  relaxed; the data-paired flush stays durable so a lost size cannot cause reads past EOF
  to return zeros (#588).

How each backend implements "durable":

| Backend | A durable commit means | A relaxed commit means |
|---|---|---|
| **Badger** | `SyncWrites=true` → the WAL is fsync'd on **every** commit. In strict mode this is where the per-commit-fsync throughput wall (#1687) bites. | `SyncWrites=false`; fsync deferred to a ~1 s background durability ticker. Only explicitly-durable paths (the flush, rollup fence) fsync inline. |
| **Postgres** | `synchronous_commit=on` — one WAL flush per COMMIT. | `SET LOCAL synchronous_commit=off` for the namespace write. |
| **SQLite** | WAL + `synchronous=NORMAL`: a COMMIT does **not** fsync; fsync happens only at WAL checkpoint. The "one fsync per commit" cost never applies; the bottleneck is the single writer connection. | same (checkpoint-only fsync). |

The upshot: the badger backend's dominant cost is fsync *frequency* (one per durable
commit), which the relational backends have already relocated off the per-commit path.
Their remaining costs are statements-per-commit and index write-amplification, not fsync
count.

---

## Known simplification work

Three read-only audits (2026-07-15) mapped the metadata store in full and feed the
cleanup tracked in **[#1715](https://github.com/marmos91/dittofs/issues/1715)** (a
follow-up to the #1687 throughput work). The audits are the roadmap for what to reduce:

- `.planning/perf/2026-07-15-metadata-schema-inventory.md` — Badger key-prefix inventory,
  what is persisted/fsync'd, and the durability model.
- `.planning/perf/2026-07-15-metadata-entity-model.md` — the Go domain structs and their
  Badger mappings.
- `.planning/perf/2026-07-15-metadata-sql-schema.md` — the Postgres + SQLite relational
  schema and its dead/redundant indexes.

The items below are **planned**, not implemented on `develop`. They are listed so a
maintainer reading this doc knows the audit exists and where the levers are:

- **Split `Blocks[]` out of the Badger `f:` blob** into a sibling key so an attr-only
  flush stops re-serializing the manifest (the relational `file_block_refs` precedent).
- **Skip the SQL manifest rewrite when `Blocks` is unchanged** — today `PutFile`
  unconditionally `DELETE`s and re-`INSERT`s `file_block_refs` even on a chmod/size bump.
- **Collapse the `nlink` duplication** in Badger to one source of truth (SQL already did).
- **Delete the vestigial `li:`/`ro:` keyspaces and their `local_chunk_index`/
  `rollup_offsets` SQL twins** — gated on the journal redesign (#1692) owning local reads.
- **Drop confirmed-dead schema**: the Badger `d:` prefix; the Postgres `pending_writes`
  table and several redundant/dead indexes (`idx_inodes_updated_at`, `idx_inodes_has_acl`,
  the redundant `parent_child_map` indexes).

See the audit docs for the full ranked analysis, before→after sketches, and risk notes.
