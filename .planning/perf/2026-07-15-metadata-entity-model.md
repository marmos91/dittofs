# Metadata Entity Model — Domain Types → Badger KV Mapping

Read-only investigation, branch `develop`, 2026-07-15. Entity-first companion to
the **storage-first** schema audit `.planning/perf/2026-07-15-metadata-schema-inventory.md`
(the "schema doc" below), which inventoried badger by KEY PREFIX. This document
does the inverse: it enumerates the Go domain structs in `pkg/metadata/**` (and
the `pkg/block` value types they embed) and maps each to how it lands in the
badger default store. Cross-references to the #1715 cleanup issue (follow-up to
#1687) avoid duplicating tracked items.

All claims cite `file:line`. **[C]** = confirmed by reading the code, **[S]** =
suspected/inferred. Persistence classes: **PERSISTED** (own badger key) /
**EMBEDDED** (rides another entity's blob) / **DERIVED** (synthesized on read,
never stored) / **TRANSIENT** (in-memory only).

---

## 1. Entity catalog

### 1.1 `File` — the aggregate root  `[C]`

- **Type:** `File` — `pkg/metadata/file_types.go:13`
- **Fields:** `ID uuid.UUID`, `ShareName string`, `Path string`, embedded `FileAttr`.
- **Persistence class:** **PERSISTED** — `f:<uuid>` (schema doc row 1).
- **Badger mapping:** whole struct JSON-marshalled into one value
  (`encodeFile` file_types→encoding.go:150-165). `Path` is **zeroed before encode**
  (encoding.go:155-159) → DERIVED on read via `derivePath`. So the persisted `f:`
  blob = `ID` + `ShareName` + the entire `FileAttr` (below).
- **CRUD:** `GetFile`/`PutFile`/`DeleteFile` on the store interface; badger impl in
  `files.go` + `transaction.go` (`PutFile` transaction.go:430). Handle encode/decode
  `EncodeFileHandle`/`DecodeFileHandle` (types.go:41,66) — `FileHandle = shareName:uuid`.

### 1.2 `FileAttr` — the attribute bag (embedded, never standalone)  `[C]`

- **Type:** `FileAttr` — `pkg/metadata/file_types.go:28`. Embedded into `File`;
  has no identity of its own.
- **Fields** (file_types.go:28-132): `Type FileType`, `Mode uint32`, `UID/GID uint32`,
  `Nlink uint32`, `Size uint64`, `Atime/Mtime/Ctime/CreationTime time.Time`,
  `PayloadID PayloadID`, `LinkTarget string`, `Rdev` (device number — see note),
  `Hidden bool`, `ACL *acl.ACL`, `EAs map[string][]byte`, `IdempotencyToken uint64`,
  `Blocks []block.ChunkRef`, `ObjectID block.ObjectID`, `DeletedAt *time.Time`,
  `OriginalPath string`, `DeletedBy string`.
- **Persistence class:** **EMBEDDED** in the `f:` blob (it is the bulk of it).
- **Badger mapping:** one JSON object inside the `f:` value. Every mutation
  re-serializes the whole thing (schema doc §"File inode"). `omitempty` on the heavy
  optional fields (`acl`, `eas`, `blocks`, recycle fields) keeps the common
  small-file blob compact, but any set of ACL/EA/Blocks bloats it.
- **Note (Rdev):** the doc comment says `Rdev uint64` but the struct body was
  garbled in the compressed read; device major/minor lives here (this is *why* the
  badger `d:` prefix is dead — §3.3). `[S]` on exact field name, `[C]` on "device
  numbers ride the blob, not a `d:` key".
- **CRUD:** same as `File`. Helper methods `LookupEA`/`ApplyEAMutations`/`findEAKey`
  (file_types.go:181-238) operate on the embedded `EAs` map.

### 1.3 `FileType` (enum) + `PayloadID` (string) — value types  `[C]`

- `FileType int` — file_types.go:241 (`FileTypeRegular…FileTypeFIFO`, iota). EMBEDDED
  scalar in `f:` blob.
- `PayloadID string` — file_types.go:254. EMBEDDED scalar in `f:` blob AND the key of
  the `pl:<payloadID>` secondary index (schema doc row `pl:`). Content identity
  `share/<inode-uuid>` (encoding.go:126-134).

### 1.4 `SetAttrs` / `EAMutation` — command DTOs, never persisted  `[C]`

- `SetAttrs` file_types.go:135, `EAMutation` file_types.go:~167.
- **Persistence class:** **TRANSIENT** — argument structs for `SetFileAttributes`;
  applied to a `FileAttr` then discarded. Not entities.

### 1.5 `block.ChunkRef` — the content manifest entry  `[C]`

- **Type:** `ChunkRef` — `pkg/block/types.go:158`. `{Hash ContentHash; Offset uint64; Size uint32}`.
- **Persistence class:** **EMBEDDED** — the `Blocks []ChunkRef` slice rides the `f:`
  File blob (file_types.go:93-103, comment states "Badger: rides existing JSON-encoded
  FileAttr blob"; "Postgres: separate file_block_refs join table").
- **Badger mapping:** JSON array inside `f:`. `ContentHash` custom-marshals to
  `"blake3:{hex}"` (types.go:98-104) with a base64/number-array legacy fallback
  (types.go:111-144). This is the **heaviest embedded field** — see §3.1 / §5.
- **CRUD:** written at sync finalization (`MergeChunkRefsByOffset` types.go:175,
  `PruneChunkRefsToSize` types.go:217 on truncate). Read on every carve/read path.

### 1.6 `block.ObjectID` — Merkle root + dedup key  `[C]`

- **Type:** alias of `ContentHash` (BLAKE3 root over sorted `ChunkRef.Hash`),
  field `ObjectID block.ObjectID` file_types.go:~118.
- **Persistence class:** **EMBEDDED** in `f:` blob **+** a **PERSISTED secondary
  index** `obj:<hex>` → fileUUID (schema doc row `obj:`, encoding.go:118-123).
- **Badger mapping:** the value in the blob is authoritative; `obj:` is a reverse
  index for `FindByObjectID`, written/deleted only when `ObjectID` changes
  (schema doc §"Secondary indexes"). All-zero sentinel = "never quiesced".
- **CRUD:** set by the post-flush quiesce hook; index maintained inside `PutFile`.

### 1.7 `acl.ACL` — NFSv4 / Windows ACL  `[C]`

- **Type:** `ACL` — `pkg/metadata/acl/types.go:186`. `{ACEs []ACE; Source ACLSource;
  Protected/AutoInherited/NullDACL bool; SACL []ACE}`.
- **Persistence class:** **EMBEDDED** — `ACL *acl.ACL` pointer field in `FileAttr`
  (file_types.go:75), `omitempty`. nil = classic Unix perms.
- **Badger mapping:** nested JSON inside `f:` blob. SQL contrast: postgres migration
  `000004_acl` adds a column. Setting an ACL re-serializes the whole inode.

### 1.8 EA / xattr — no entity, just the embedded map  `[C]`

- **Type:** no dedicated struct; it is `EAs map[string][]byte` on `FileAttr`
  (file_types.go:88). `EAMutation` (§1.4) is the mutation command.
- **Persistence class:** **EMBEDDED** in `f:` blob (there is **no** xattr keyspace —
  confirmed: `SetXattr` RMWs the whole File, badger/xattr.go is 2.1K and defers to
  the blob; schema doc note confirms). SQL contrast: postgres `eas JSONB` column
  (migration `000028`).

### 1.9 `Share` (+ `ShareOptions`, `IdentityMapping`) and `shareData`/RootHandle  `[C]`

- **Types:** `Share` types.go:114, `ShareOptions` types.go:127, embedded
  `IdentityMapping *IdentityMapping` (identity pkg). Badger-internal wrapper
  `shareData{Share; RootHandle FileHandle}` — **encoding.go:141-144**.
- **Persistence class:** **PERSISTED** — `s:<shareName>` → `shareData` JSON
  (schema doc row `s:`).
- **Badger mapping:** `Share` + its root-dir `FileHandle` co-located in one blob
  (`encodeShareData` encoding.go:175). `GetRootHandle` reads `data.RootHandle`
  (shares.go:31-55); `CreateRootDirectory` back-fills it (shares.go:678,
  `shareData{…RootHandle: rootHandle}`). RootHandle is EMBEDDED in the share blob,
  not a separate key.
- **CRUD:** `AddShare`/`UpdateShare`/`RemoveShare`/`GetRootHandle` — badger `shares.go`.

### 1.10 `MetadataServerConfig` — server-wide settings  `[C]`

- **Type:** `MetadataServerConfig{CustomSettings map[string]any}` — types.go:185.
- **Persistence class:** **PERSISTED** — `cfg:server` JSON (schema doc row `cfg:`).
- **Alive?** Yes — `CustomSettings` is read/written by the block-GC reconcile marker
  (`blockgc_reconcile.go:237,257`) and exercised by the conformance suite. Not dead.
- **CRUD:** `GetServerConfig`/`SetServerConfig`.

### 1.11 `FilesystemCapabilities` + `FilesystemStatistics` + `FilesystemMeta`  `[C]`

- **Types:** `FilesystemCapabilities` types.go:203 (static limits/features),
  `FilesystemStatistics` types.go:292 (dynamic usage), combined by
  `FilesystemMeta{Capabilities; Statistics}` — **store.go:339**.
- **Persistence class (split brain):**
  - `FilesystemCapabilities` → **PERSISTED** `cap:fs` JSON (schema doc row `cap:`).
  - `FilesystemMeta` → **PERSISTED** `fsmeta:<share>` JSON
    (`prefixFilesystemMeta` transaction.go:1015; schema doc row `fsmeta:`).
  - `FilesystemStatistics` alone → **DERIVED** — recomputed per-statfs, e.g.
    `transaction.go:1520` builds `FilesystemStatistics{…}` on the fly (also
    memory/sqlite/postgres statfs.go). Usage counters are rebuilt from `f:` rows at
    startup (schema doc "Quota usage" row).
- **Collapsible:** `FilesystemMeta` is a 2-field wrapper whose `.Capabilities` half
  duplicates the standalone `cap:fs` entity — see §3.4 / §5.

### 1.12 `block.FileChunk` — the block-store DTO (DERIVED in badger)  `[C]`

- **Type:** `FileChunk` — `pkg/block/types.go:240`. Rich per-chunk record
  (`ID, Hash, DataSize, LocalPath, BlockStoreKey, RefCount, LastAccess,
  LastSyncAttemptAt, CreatedAt, State`).
- **Persistence class:** **DERIVED** in badger — there is **no `fc:` key**. The badger
  `FileChunkStore` methods (`GetFileChunk`, `GetByHash`, `ListFileChunks`,
  `GetFileChunkAtOffset` — objects.go:72,347,400,456) *synthesize* a `FileChunk` by
  projecting the file's `Blocks []ChunkRef` (from `f:`) plus block-lifecycle state
  from `br:`/`synced:`. SQL contrast: postgres has a real `file_blocks` table
  (`file_block_refs.go:123 InsertNullHashFileChunk`, migrations 000010/000011).
- **Consequence:** `FileChunk` is a projection type, not a stored entity, in badger —
  its fields `LocalPath`/`BlockStoreKey`/`RefCount` map to different badger keys, not
  one row. This is the single biggest badger↔SQL modelling divergence (§4).

### 1.13 `block.BlockRecord` — packed-block lifecycle  `[C]`

- **Type:** `BlockRecord` — `pkg/block/block_record.go:4`.
  `{BlockID string; BlockHash ContentHash; Length int64; LiveChunkCount uint32; SyncState BlockState}`.
- **Persistence class:** **PERSISTED** — `br:<blockID>` JSON (schema doc row `br:`;
  `prefixBlockRecord = "br:"` block_record_store.go:32).
- **Badger mapping:** standalone JSON value, written via bare `s.db.Update`
  (CommitBlock / GC-decrement path). Consumed by the engine GC. KEEP.

### 1.14 `block.LocalChunkLocation` — local log-blob index  `[C]`  **VESTIGIAL-after-journal**

- **Type:** `LocalChunkLocation` — `block_record.go:13`.
  `{LogBlobID string; RawOffset int64; RawLength int64}`.
- **Persistence class:** **PERSISTED** — `li:<hex(hash)>` JSON
  (`prefixLocalChunkIdx = "li:"` block_record_store.go:33,44).
- **Badger mapping:** bare `s.db.Update` per unique local chunk → **1 `db.Sync`/chunk
  in strict mode** (schema doc row `li:`). Consumed only by `pkg/block/local/fs/`.
- **Verdict:** vestigial once the journal (#1692) owns local-read location. Tracked in
  #1715 — do not duplicate.

### 1.15 `block.ChunkLocator` + synced marker — remote location  `[C]`

- **Type:** `ChunkLocator` — `pkg/block/locator.go:13`.
  `{BlockID string; WireOffset int64; WireLength int64}`. `IsStandalone()` = legacy
  zero form (locator.go:28).
- **Persistence class:** **PERSISTED** as the *value suffix* of the synced marker:
  `synced:<32-byte-hash>` → `nanos(8B) + ChunkLocator` (`syncedHashPrefix = "synced:"`
  synced_hash_store.go:48,97; schema doc row `synced:`). Length-prefixed binary head
  (8 nanos) + encoded locator.
- **Badger mapping:** PERSISTED standalone key; the marker *is* the "chunk is mirrored
  remotely" fact. Consumed by engine sync + GC. KEEP.
- **`BlockChunkCommit`** (block_record.go:19) `{Hash; Remote ChunkLocator; Local
  LocalChunkLocation}` — **TRANSIENT** commit-bundle DTO, not persisted.

### 1.16 `block.ContentHash` / `BlockState` — value types  `[C]`

- `ContentHash [32]byte` types.go:24 — EMBEDDED everywhere (in `ChunkRef`, `ObjectID`,
  `BlockRecord`, synced key). Custom JSON = `"blake3:{hex}"`.
- `BlockState uint8` types.go:70 (`Pending/Syncing/Remote`) — EMBEDDED scalar in
  `BlockRecord`/`FileChunk`.

### 1.17 Rollup offset — `ro:` fence  `[C]`  **VESTIGIAL-after-journal**

- **Type:** no struct — bare `uint64 LE`. Key `ro:<payloadID>`
  (`rollupOffsetPrefix = "ro:"` rollup.go:34,41).
- **Persistence class:** **PERSISTED** scalar. `SetRollupOffset` forces an fsync even in
  relaxed mode (schema doc row `ro:`). Consumed only by `pkg/block/local/fs/`.
- **Verdict:** vestigial with `li:` once the journal lands. Tracked in #1715.

### 1.18 Lock / NSM / durable-handle / recycle entities (catalog only)

Per brief, these are listed but **not analyzed** (another session owns lock/NLM/NSM
semantics):

- **NLM/lease lock** — value structs persisted under `lock:` `lkfile:` `lkowner:`
  `lkclient:` `srvepoch` (badger/locks.go; store type `badgerLockStore` locks.go:71).
  PERSISTED, lock JSON + indexes. `[C]` on prefixes (schema doc row `lock:`).
- **NSM client** — `nsm:client:` `nsm:monname:` (badger/clients.go; `badgerClientStore`
  clients.go:41). PERSISTED client JSON. `[C]`.
- **SMB durable handle** — `dh:id:` `dh:cguid:` `dh:appid:` `dh:fid:` `dh:share:`
  (badger/durable_handles.go; `badgerDurableStore` durable_handles.go:40). PERSISTED
  handle JSON + indexes, SMB path only (not NFS hot path). `[C]`.
- **Recycle/trash** — no standalone entity: `DeletedAt`/`OriginalPath`/`DeletedBy`
  are EMBEDDED fields on `FileAttr` (file_types.go:120-131); trash listing scans `f:`
  rows with non-nil `DeletedAt`. Value-object state, not its own key. `[C]`.
- **Snapshot/backup** — no live keyspace; `WriteSnapshot`/`ReadSnapshot` stream a
  length-prefixed dump (badger/snapshot_store.go). No `SnapshotPolicy` struct in
  `pkg/metadata` (grep empty). `[C]`.

### 1.19 `ShareSession` — TRANSIENT (memory-only)  `[C]`

- **Type:** `ShareSession{ShareName; ClientAddr; MountedAt}` — types.go:161.
- **Persistence class:** **TRANSIENT** — used *only* by the memory store's in-RAM
  `sessions map` (memory/store.go:201-203,355; reset.go:35). **Never touched by the
  badger store** (grep: zero badger hits). It is a mount-tracking value object, not a
  persisted entity. Candidate to drop from `pkg/metadata` core types if the memory
  store is the sole user — see §3.3.

---

## 2. Relationship map (ER view)

Text ER (badger realization in brackets):

```
        Share ──1:1── RootHandle            [EMBEDDED in s: blob (shareData.RootHandle)]
          │
          │ 1:N  (routing by ShareName prefix of handle)
          ▼
        File(f:<uuid>) ──1:1── FileAttr      [EMBEDDED: FileAttr is the f: blob body]
          │  │  │  │  │  │
          │  │  │  │  │  └─ 0:N EAs           [EMBEDDED map in f: blob]
          │  │  │  │  └──── 0:1 ACL           [EMBEDDED *acl.ACL in f: blob]
          │  │  │  └─────── 0:N Blocks[]ChunkRef [EMBEDDED slice in f: blob]
          │  │  │                              │
          │  │  │                              └─ ObjectID = Merkle(Blocks) [EMBEDDED + obj: reverse index]
          │  │  └─ nlink ──────────────────── [DUPLICATED: FileAttr.Nlink AND l:<uuid>]
          │  └─── PayloadID ───────────────── [EMBEDDED + pl: reverse index]
          │
          ├─ parent edge          [SEPARATE KEY p:<childUUID> → parentUUID]
          ├─ child edge           [SEPARATE KEY c:<parentUUID>:<name> → childUUID]
          └─ child-name rev edge  [SEPARATE KEY cn:<parentUUID>:<childUUID> → name]

   ChunkRef.Hash ──resolves──► synced:<hash> → ChunkLocator   [remote location, SEPARATE KEY]
                          └──► li:<hash> → LocalChunkLocation  [local location, SEPARATE KEY]
   packed block ─────────────► br:<blockID> → BlockRecord      [SEPARATE KEY]
   FileChunk = PROJECTION of (ChunkRef ⨝ br: ⨝ synced:)        [DERIVED, no key]
```

| Relationship | Cardinality | Encoding | Keys involved |
|---|---|---|---|
| Share → RootHandle | 1:1 | **EMBEDDED** in share blob | `s:<name>` (shareData.RootHandle) |
| Share → Files | 1:N | implicit (handle carries ShareName) | `f:` handle prefix |
| File → FileAttr | 1:1 | **EMBEDDED** (same blob) | `f:<uuid>` |
| File → parent | N:1 | **SEPARATE edge key** | `p:<childUUID>` |
| Dir → children | 1:N | **SEPARATE edge keys** | `c:<parentUUID>:<name>` |
| Dir → child names | 1:N | **SEPARATE reverse edge** (derivePath O(1), #1166) | `cn:<parentUUID>:<childUUID>` |
| File → nlink | 1:1 | **SEPARATE key + DUPLICATED in blob** | `l:<uuid>` + `FileAttr.Nlink` |
| File → PayloadID | 1:1 | **EMBEDDED + SEPARATE index** | blob + `pl:<payloadID>` |
| File → Blocks | 1:N | **EMBEDDED slice** | inside `f:` blob |
| File → ObjectID | 1:1 | **EMBEDDED + SEPARATE index** | blob + `obj:<hex>` |
| File → ACL | 0:1 | **EMBEDDED** | inside `f:` blob |
| File → EAs | 0:N | **EMBEDDED map** | inside `f:` blob |
| ChunkRef → remote loc | 1:1 | **SEPARATE key** | `synced:<hash>` → ChunkLocator |
| ChunkRef → local loc | 1:1 | **SEPARATE key** | `li:<hash>` → LocalChunkLocation |
| Block → record | 1:1 | **SEPARATE key** | `br:<blockID>` |
| PayloadID → rollup fence | 1:1 | **SEPARATE scalar** | `ro:<payloadID>` |

**Read:** File's structural relationships (parent/child/nlink/objectid/payload) are
realized as **separate edge/index keys**; File's *content and attribute* relationships
(FileAttr, ACL, EAs, Blocks, recycle state) are **embedded** in the one `f:` blob.

---

## 3. Normalization observations

### 3.1 EMBEDDED-but-heavy: `Blocks[]` (+ ACL/EAs) in the `f:` blob  `[C]`

`Blocks []ChunkRef` and, when present, `ACL`/`EAs` all ride the `f:` File JSON
(file_types.go:75,88,103). Any inode mutation — including a hot CLOSE flush that only
bumps `Size`+`Mtime`+`Ctime` — re-serializes the entire blob, `Blocks` included. This
is the write-amplification the schema doc's `fb:<uuid>` split targets (schema doc §5
item 2; #1715). SQL already normalizes this: `file_block_refs` join table
(migration 000012, "we use a separate table (not JSONB on files) to avoid TOAST write
amplification") — so the badger embedding is **accidental**, not intrinsic.

### 3.2 DUPLICATED: `Nlink` in two places  `[C]`

`FileAttr.Nlink` (file_types.go:42) is persisted in the `f:` blob **and** authoritatively
in the `l:<uuid>` key; `GetFile` overrides the blob value from `l:` (schema doc row `l:`).
**SQL already resolved this the opposite way**: postgres migration `000034_drop_link_counts`
*dropped* the separate `link_counts` table and made the `nlink` *column* the sole source
of truth. Badger is the only backend still carrying both — a live drift source. See §5.

### 3.3 DERIVED-not-stored  `[C]`

- `File.Path` — zeroed before encode (encoding.go:155-159), rebuilt via `derivePath`.
- `FilesystemStatistics` — recomputed per statfs (transaction.go:1520), never a stored
  key on its own (only the combined `fsmeta:` blob persists a snapshot).
- `block.FileChunk` — projection over `f:`.Blocks + `br:` + `synced:` (§1.12); no `fc:` key.
- **DEAD-confirmed:** badger `d:` device-number prefix — only the doc comment
  (encoding.go:40) and the const (encoding.go:51); **no key function, no getter/setter**
  (grep: `eviceNumber` → those two lines only). Device numbers live in the `f:` blob.
  (Same finding as schema doc; tracked #1715.)
- **TRANSIENT/memory-only:** `ShareSession` (§1.19) — used only by the memory store.

### 3.4 Value-object vs entity  `[C]`

- **Entities (have a key/identity):** `File` (`f:`), `Share` (`s:`),
  `MetadataServerConfig` (`cfg:`), `FilesystemCapabilities` (`cap:`), `FilesystemMeta`
  (`fsmeta:`), `BlockRecord` (`br:`), `LocalChunkLocation` (`li:`), synced marker
  (`synced:`), lock/NSM/durable structs.
- **Value objects (embedded, no identity):** `FileAttr`, `ChunkRef`, `ObjectID`,
  `ACL`, EA map, `ContentHash`, `BlockState`, `FileType`, `PayloadID`, `ChunkLocator`
  (rides the synced value), recycle fields, `RootHandle` (rides share).
- **Command DTOs (transient):** `SetAttrs`, `EAMutation`, `BlockChunkCommit`.
- **Projection DTO (derived):** `FileChunk`.
- **Wrapper worth questioning:** `FilesystemMeta` — a 2-field bundle
  (`Capabilities`+`Statistics`) whose Capabilities half duplicates the standalone
  `cap:fs` entity, and whose Statistics half is otherwise DERIVED. Collapsible (§5).

---

## 4. Badger-vs-SQL contrast (the illuminating four)

| Entity | Badger | Postgres/SQLite | Verdict |
|---|---|---|---|
| **Blocks (ChunkRef[])** | EMBEDDED JSON array in `f:` blob (file_types.go:103) | `file_block_refs` join table, PK `(file_id, "offset") INCLUDE(size,hash)`, FK CASCADE (migration 000012) | badger embedding **accidental** — SQL split it deliberately to dodge write amplification |
| **nlink** | `l:<uuid>` key **and** blob field (dual) | single `nlink` column; `link_counts` table **dropped** (migration 000034) | badger duplication **accidental** — SQL already collapsed to one source |
| **ObjectID** | EMBEDDED in blob + `obj:<hex>` reverse index | `object_id BYTEA` column + partial UNIQUE index (migration 000013, "naturally one-to-one … read on every GetFile alongside rest of row") | badger split (blob+index) is **intrinsic/fine** — SQL keeps it in-row too; the reverse-lookup index exists in both |
| **EAs / ACL** | EMBEDDED nested JSON in `f:` blob | `eas JSONB` column (000028), ACL column (000004) | **intrinsic** — both keep them with the row; SQL just gets column-level partial rewrite, badger rewrites whole blob |
| **FileChunk** | DERIVED projection (no key) | real `file_blocks` table (000010/000011) | modelling divergence — badger synthesizes what SQL stores |

Takeaway: the two embeddings SQL *deliberately normalized away* (`Blocks`, `nlink`) are
the two badger cleanup wins; ObjectID/EAs/ACL in-blob are intrinsic and match SQL's
in-row choice.

---

## 5. Cleanup / normalization candidates (ranked, concrete, before→after)

Cross-references to #1715 are marked; items already tracked there are **not** re-proposed
in full — only entity-model framing is added. New items are marked **[NEW]**.

**1. Split `Blocks []ChunkRef` out of the `f:` blob → `fb:<uuid>` sibling key.**
   *(Already #1715 item — entity framing only.)*
   - Before: `f:<uuid>` = File+FileAttr+**Blocks**+ACL+EAs (one blob, full rewrite on
     every size/mtime bump).
   - After: `f:<uuid>` = File+FileAttr+ACL+EAs; `fb:<uuid>` = `[]ChunkRef` written only
     at sync finalization.
   - Effect: hot CLOSE flush stops re-serializing the manifest → smaller WAL fsync
     payload, fewer `ensureRoomForWrite` trips. Risk: **medium** (encoding + every File
     reader + read shim/migration). SQL precedent: `file_block_refs`.

**2. Kill the `Nlink` duplication — one source of truth. [NEW entity-model item]**
   - Before: `FileAttr.Nlink` in the `f:` blob **and** `l:<uuid>` key; `GetFile`
     overrides blob←`l:`.
   - After (preferred, matches SQL 000034): **drop the `l:<uuid>` keyspace**, make the
     blob's `Nlink` authoritative — SQL already proved the column-as-truth model works
     and removed the side table. Alternative (smaller diff): keep `l:` authoritative,
     stop persisting `Nlink` in the blob (encode `-`/omit).
   - Effect: removes a dual-write and a drift source; one fewer key touched per
     create/link/unlink txn (they already ride one txn so **not** a sync-count win, but a
     write-volume + correctness win). Risk: **low-medium** — grep every reader that trusts
     `File.Nlink` without the `l:` override before flipping which side wins.
   - Note: this is the badger analogue of postgres migration 000034; **not** currently in
     #1715 (which focuses on sync frequency). Worth adding.

**3. Delete the dead `d:` device-number prefix.** *(Already #1715 — confirmed dead here
   too, §3.3.)* Pure dead-code removal, zero runtime effect.

**4. Delete `li:` (LocalChunkLocation) + `ro:` (rollup fence) keyspaces after the
   journal switchover.** *(Already #1715, gated on #1692.)* Entity framing: both
   `LocalChunkLocation` and the bare `ro:` scalar become DERIVABLE from the journal's own
   `.idx`. Biggest sync-frequency win; do not delete on develop before #1692 lands.

**5. Collapse `FilesystemMeta` / `FilesystemCapabilities` / `cap:fs` overlap. [NEW]**
   - Before: `FilesystemCapabilities` persists standalone at `cap:fs`; `FilesystemMeta`
     (store.go:339) also embeds a `Capabilities` copy inside `fsmeta:<share>`, and its
     `Statistics` half is otherwise DERIVED (transaction.go:1520).
   - After: keep `cap:fs` as the single capabilities entity; make `fsmeta:` store only
     the dynamic statistics (or drop `FilesystemMeta` as a persisted type and compute
     statfs on the fly, which is already what the read path does).
   - Effect: removes a duplicated capabilities blob and a 2-field wrapper type. Risk:
     **low** (control-plane path, rare writes). Not in #1715.

**6. Promote/normalize `ShareSession` out of core types, or drop it. [NEW, trivial]**
   - `ShareSession` (types.go:161) is used only by the memory store's RAM map; badger
     never persists it. Either move it into `store/memory` (it is a memory-store detail,
     not a domain entity) or delete if mount-tracking is vestigial.
   - Effect: shrinks the `pkg/metadata` domain surface by one non-entity. Risk: **low**.
     Not in #1715.

**7. Tighten hot scalar encodings (optional, low priority). [NEW]**
   - `synced:<hash>` value is `8-byte nanos + ChunkLocator` — already binary/compact,
     KEEP. But the `f:` blob is JSON; if #1 (fb: split) lands, the residual attr-only
     blob is a candidate for a fixed binary layout (`Mode/UID/GID/Size/times` are all
     fixed-width) to shrink the hot-flush WAL payload further. Effect: smaller hot
     commit; Risk: **high** (touches every reader + migration) — only worth it if #1
     alone doesn't move the wall.

Ordering rationale: #1/#4 are the throughput levers (payload size / sync frequency,
already in #1715); #2/#3 are correctness+dead-code hygiene with SQL precedent; #5/#6 are
domain-surface simplifications; #7 is a speculative follow-on gated on #1.
