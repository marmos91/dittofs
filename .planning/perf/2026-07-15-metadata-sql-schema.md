# SQL Metadata Stores (Postgres + SQLite) — Schema / Hot-Path Inventory

Read-only investigation, branch `develop`, 2026-07-15. Third companion to the two
badger passes:
- `.planning/perf/2026-07-15-metadata-schema-inventory.md` (badger, storage-first)
- `.planning/perf/2026-07-15-metadata-entity-model.md` (Go structs → badger)

**[C]** = confirmed by reading code/migration. **[S]** = suspected/inferred.

## TL;DR — the SQL wall is NOT db.Sync frequency

The badger wall is per-COMMIT WAL fsync. **Both SQL backends already relocated that
cost off the per-commit path**, so the levers are different:

- **SQLite** opens with `journal_mode=WAL` + `synchronous=NORMAL` (config.go:91). In
  WAL+NORMAL a `COMMIT` does **not** fsync — fsync happens only at WAL checkpoint.
  So the "one fsync per commit" mapping that dominates badger **does not hold for
  sqlite**; the known "sqlite end-of-write fsync" concern is already mitigated. The
  real sqlite wall is the **single writer**: `db.SetMaxOpenConns(1)` (store.go:108)
  serializes every write txn behind one connection. **[C]**
- **Postgres** defaults to `synchronous_commit=on` (one WAL flush per COMMIT), but
  `RelaxedDurability` issues `SET LOCAL synchronous_commit=off` for namespace writes
  (transaction.go:117,161; config.go:36-43), matching badger relaxed mode. Data-paired
  writes (flush) stay synchronous (#588). **[C]**

Because commit *count* per file op is driven by the **backend-agnostic** service layer
(`pkg/metadata/io.go` + `service.go` — the same `WithTransaction` calls badger uses),
the SQL-specific wins are: **(1) statements per commit / row churn**, **(2) index
write-amplification**, **(3) dead tables**. The single biggest SQL-only finding is that
**every flush/setattr rewrites the entire `file_block_refs` manifest** (lever H1 below).

Both stores are **hand-written raw SQL** (pgx / database/sql), **not GORM** — the two
"gorm" hits (sqlite/migrate.go:22, sqlite/store.go:14) are comments noting the
control-plane GORM layer shares the driver. **GORM footguns (bool default:true,
OnConflict reload, UpdateShare full-column map) do NOT apply to these backends.** **[C]**

---

## 1. Table / index inventory

Legend: KEEP · DEAD (no reader/writer) · VESTIGIAL (journal #1692 obsoletes) · REDUCIBLE
(redundant index / write tax). Migration numbers are postgres unless noted; sqlite
collapses postgres 000001–000038 into 5 migrations (sqlite/000001 = the consolidated
final state; there are no legacy sqlite deployments — sqlite/000001 comment).

### `inodes` (postgres `files`, renamed 000032; path/path_hash dropped 000031/000032, #1166)

| Column | pg | sqlite | Verdict / note |
|---|---|---|---|
| id, share_name, file_type, mode, uid, gid, size, nlink | ✓ | ✓ | KEEP |
| atime/mtime/ctime/creation_time | BIGINT FILETIME (000023→000038) | INTEGER FILETIME (000005) | KEEP |
| content_id | ✓ (+content_id_hash, md5, 000001) | ✓ (no hash col — no btree size limit) | KEEP; **pg content_id_hash + its trigger is a pg-only workaround** |
| link_target, device_major, device_minor, hidden | ✓ | ✓ | KEEP |
| acl JSONB (000004) / acl TEXT | ✓ | ✓ | KEEP (in-row, matches badger) |
| eas JSONB (000028) / eas TEXT | ✓ | ✓ | KEEP (in-row) |
| object_id BYTEA (000013) / BLOB | ✓ | ✓ | KEEP |
| deleted_at/original_path/deleted_by (000027) | ✓ | ✓ | KEEP (recycle, in-row) |
| created_at, updated_at | ✓ | ✓ | **updated_at REDUCIBLE — see below** |
| ~~path, path_hash~~ | dropped 000031/000032 | never existed | already gone |

**Indexes on `inodes`:**

| Index | Migration | pg | sqlite | Used by hot read? | Verdict |
|---|---|---|---|---|---|
| PK (id) | 000001 | ✓ | ✓ | GetFile, PutFile, GetParent join | KEEP |
| idx_inodes_share_name | 000001 | ✓ | ✓ | share enumeration / reset (rare) | KEEP (low) |
| idx_inodes_content_id_hash (pg) / idx_inodes_content_id (sqlite) | 000001 | ✓ | ✓ | GetFileByPayloadID (`WHERE content_id=`) | KEEP |
| inodes_object_id_idx (partial UNIQUE) | 000013 | ✓ | ✓ | FindByObjectID + dedup first-committer-wins | KEEP |
| idx_inodes_updated_at | 000001 | ✓ | ✓ | **NONE — grep: no `ORDER BY/WHERE updated_at`** | **DEAD-index → REDUCIBLE (pure write tax)** |
| idx_inodes_hidden (partial WHERE hidden) | 000001 | ✓ | ✓ | SMB hidden listing [S] | KEEP-verify (low value, partial→cheap) |
| idx_inodes_has_acl (partial) | 000004 | ✓ | ✗ | **NONE — grep: no `acl IS NOT NULL` query** | **pg-only DEAD-index → drop** |
| idx_inodes_uid / idx_inodes_gid (partial WHERE file_type=0) | 000033 | ✓ | ✓ | quota seed scan **at startup only** | KEEP-verify (taxes every regular-file insert/chown for a startup-only reader) |

**Postgres-only triggers on `inodes`** (no sqlite equivalent):
- `update_inodes_updated_at` BEFORE UPDATE → `updated_at=NOW()` on **every** inode
  UPDATE (000001). Feeds only the dead `idx_inodes_updated_at`. **REDUCIBLE (drop with
  the index).** sqlite never maintains updated_at on UPDATE (divergence — sqlite is
  cheaper here). **[C]**
- `inodes_content_id_hash_trigger` BEFORE INSERT/UPDATE OF content_id → md5 (000001).
  Alive (feeds the used content_id_hash index) but pg-only overhead sqlite avoids. KEEP.

### `parent_child_map` (the namespace)

| Index | Migration | pg | sqlite | Verdict |
|---|---|---|---|---|
| PK (parent_id, child_name) | 000001 | ✓ | ✓ | KEEP (GetChild, SetChild, ListChildren all served by this) |
| idx_parent_child_map_parent (parent_id) | 000001 | ✓ | ✓ | **REDUCIBLE — redundant with PK leftmost prefix** |
| idx_parent_child_map_parent_name (parent_id, child_name) | 000001 | ✓ | ✗ | **pg-only, DEAD — IDENTICAL to PK** |
| idx_parent_child_map_child (child_id) | 000001 | ✓ | ✓ | KEEP (GetParent reverse lookup, hard links) |

Confirmed by reading `ListChildren` (transaction.go:658 `WHERE dc.parent_id=$1 AND
dc.child_name>$2 ORDER BY child_name`) and `GetChild` (transaction.go:555 `WHERE
parent_id AND child_name`) — both use the PK. `idx_parent_child_map_parent` and the pg
`_parent_name` index add nothing for reads but are maintained on **every** create /
mkdir / rename / unlink. **[C]**

### `file_block_refs` (the per-file block manifest — SQL's normalized `Blocks[]`)

Created 000012 (pg), sqlite/000001. Deliberately a side table (not JSONB) to dodge
TOAST/blob write amplification (000012 comment) — this is the normalization badger's
`f:` blob lacks.

| Index | pg | sqlite | Verdict |
|---|---|---|---|
| PK (file_id, "offset") [pg INCLUDE(size,hash)] | ✓ | ✓ | KEEP (covering index-only read on cold path, pg) |
| idx_file_block_refs_hash (hash) | ✓ | ✗ | pg: KEEP (dedup live-set / BSCAS lookup, 000012). **sqlite MISSING — potential seq-scan if the same path runs on sqlite [S]** |
| idx_file_block_refs_file_id (file_id) | ✗ | ✓ | **sqlite-only REDUCIBLE — redundant with PK prefix (file_id, offset)** |

Row-churn behavior analyzed in §2/§H1.

### `file_blocks` (block lifecycle / cache — the FileChunk row)

Created 000010, non-unique hash 000011, sqlite/000001. Columns: id, hash, data_size,
cache_path, block_store_key, ref_count, last_access, created_at, state,
last_sync_attempt_at. Indexes (identical pg/sqlite): `idx_file_blocks_hash` (dedup),
`idx_file_blocks_pending` (syncer claim, partial state=0), `idx_file_blocks_remote`
(LRU evict, partial state=2), `idx_file_blocks_unreferenced` (GC, partial ref_count=0),
`idx_file_blocks_syncing_age` (janitor, partial state=1). All five partial indexes have
live readers in the syncer/GC/evict paths → **KEEP**. **[C]** (each write touches the
partial index only when the row matches the predicate, so cost is bounded.)

### `block_records` + `local_chunk_index` (SQL analogues of badger `br:` / `li:`)

Created pg 000036, sqlite/000003. `block_records(block_id PK, block_hash, length,
live_chunk_count, sync_state)`; `local_chunk_index(hash PK, log_blob_id, raw_offset,
raw_length)`. **`local_chunk_index` is the exact SQL twin of badger `li:` —
VESTIGIAL-after-journal (#1692)**, consumed only by the local log-blob engine.
`block_records` = badger `br:`, KEEP (engine GC) until journal owns it. **[C]** — same
verdict/cross-ref as #1715 badger `li:`; do not delete on develop before #1692.

### `rollup_offsets` (SQL analogue of badger `ro:`)

Created pg 000009, sqlite/000001. `(payload_id PK, rollup_offset)`. Monotone via
`ON CONFLICT DO UPDATE WHERE rollup_offset <= EXCLUDED` (000009 comment). **VESTIGIAL
-after-journal** (same as badger `ro:`; the journal's `.idx` becomes the fence).
Cross-ref #1715. **[C]**

### `synced_hashes` (remote-durable marker)

Created pg 000015, block locator cols 000035, sqlite/000001+000002.
`(hash PK, synced_at, block_id, block_offset, block_length)`. KEEP (engine sync + GC).
The three block_* locator columns are NULL for standalone CAS objects (000035 comment).

### Singletons / control-plane (rare writes — inventory only)

- `shares` (000001, +block_layout 000014): PK share_name. KEEP.
- `server_config` (000001, +store_id 000008): singleton. KEEP.
- `filesystem_capabilities` (000001) singleton; `filesystem_meta` (sqlite/000001,
  per-share). KEEP. Note pg keeps caps as a singleton row; the badger doc's
  `FilesystemMeta`/`cap:` collapse candidate does not map cleanly to SQL (already split).
- `server_epoch` (000002, +clean_shutdown 000024): singleton. KEEP.
- `v4_client_recovery` (000025): KEEP.

### `pending_writes` (postgres only) — **DEAD TABLE**

Created 000001 (`operation_id PK, file_id FK, new_size, new_mtime, content_id,
pre_write_attr JSONB, created_at`) + `idx_pending_writes_file_id` +
`idx_pending_writes_created_at`. **No production reader or writer:** grep finds zero
`FROM/INTO/UPDATE/DELETE pending_writes`; the only Go reference is snapshot_store.go:56
listing it in the reset/truncate table set. **sqlite never created it.** The two-phase
"pending write" is entirely in-RAM (`io.go` `pendingWrites`, service.go:49). **CONFIRMED
DEAD → drop table + 2 indexes** (and remove from the snapshot truncate list). **[C]**

### Lock / NSM / durable-handle tables (owned by another session — inventory only)

- `locks` (000002; +cols 000018/000019/000020/000030; sqlite/000001): indexes
  idx_locks_file_id, idx_locks_owner_id (pg only), idx_locks_client_id,
  idx_locks_share_name. Not analyzed (lock semantics owned elsewhere).
- `nsm_client_registrations` (000003): idx on callback_host+registered_at (pg) /
  mon_name (both). Not analyzed.
- `durable_handles` (000005; +many cols through 000037; sqlite/000001+000004): 6 indexes
  (create_guid UNIQUE, app_instance_id, file_id UNIQUE(pg)/non-unique(sqlite),
  share_name, metadata_handle, disconnected_at). SMB path only, not NFS hot path. Not
  analyzed.

---

## 2. Per-file-op statement + commit map

Commit boundaries are backend-agnostic (`pkg/metadata/io.go` / `service.go` →
`WithTransaction` = one pg `pool.Begin/Commit` or one sqlite `BeginTx/Commit`). What
differs by backend is **statements per commit** and **fsync-per-commit** (pg=yes unless
relaxed; sqlite=no, WAL+NORMAL). Statement counts below are per `WithTransaction`.

| File op | Commits (WithTransaction) | Statements inside PutFile / op | file:line |
|---|---|---|---|
| **CREATE** (reg file) | 1 relaxed (createEntry) | INSERT inodes; SetChild INSERT parent_child_map; SetParent(no-op); nlink via inodes col; +putFileChunkRefs DELETE (blocks empty → no INSERT) | PutFile pg transaction.go:429-452, refs file_block_refs.go:32 |
| **WRITE** (4 KiB, deferred default) | **0 store writes** (buffered in RAM `pendingWrites`) unless SUID set | none (SUID case: 1 extra durable WithTransaction, UPDATE inodes mode) | io.go:221-263 |
| **CLOSE / flush** | 1 durable (strict, #588) | GetFile (1 SELECT, folds blocks); **PutFile = CTE UPDATE inodes (1) + putFileChunkRefs DELETE(1)+INSERT×M** | io.go:434-457; PutFile pg 383/442; **refs DELETE+INSERT×M pg file_block_refs.go:33-52 (pgx.Batch, 1 round-trip), sqlite:32-48 (M separate Exec)** |
| **MKDIR** | 1 relaxed | INSERT inodes; SetChild INSERT pcm; parent nlink UPDATE inodes | PutFile+SetChild transaction.go:570-597 |
| **SETATTR** (chmod/chown/utimes) | 1 (relaxed for pure attr; durable if size) | GetFile SELECT; **PutFile CTE UPDATE inodes (1) + putFileChunkRefs DELETE+INSERT×M even though manifest unchanged** | file_modify.go:548; PutFile 476 |
| **UNLINK** | 1 | DeleteChild DELETE pcm (1); nlink UPDATE or DeleteFile DELETE inodes (1, FK-cascades file_block_refs + pcm) | DeleteChild 618, DeleteFile 513 |
| **RENAME** | 1 | GetChild SELECT; DeleteChild DELETE pcm; SetChild INSERT pcm; (SetParent no-op on inode) — namespace-only, no path column to touch (#1166) | 555/618/597 |

**Key observations:**
1. **`putFileChunkRefs` runs on EVERY PutFile for a regular file** (pg:476, sqlite:407),
   unconditionally doing `DELETE FROM file_block_refs WHERE file_id` + re-INSERT of the
   *entire* current `file.Blocks`. Since `flushPendingWrite` (io.go:434) and setattr both
   `GetFile` (which fully populates `Blocks` via `blockRefsAggExpr`) → mutate size/mode →
   `PutFile`, **an attr-only or size-only change rewrites the whole manifest.** A chmod or
   close on an M-chunk file = DELETE M rows + INSERT M rows of unchanged data. **[C] —
   this is the #1 SQL-specific write-amp (H1).**
2. **sqlite block-ref insert is row-at-a-time** (M separate `tx.Exec`, file_block_refs.go:41),
   pg uses `pgx.Batch` (one round-trip, file_block_refs.go:46). On the single sqlite
   connection this M-statement loop serializes; combined with #1 it is the sqlite carve
   hot path.
3. WRITE itself touches the store **zero** times in deferred mode (default) — RAM only.

---

## 3. Index audit

**Used (KEEP):** PK(inodes.id), content_id(_hash), object_id partial-unique, PK
parent_child_map(parent_id,child_name), idx_parent_child_map_child, PK
file_block_refs(file_id,offset), pg idx_file_block_refs_hash, all five file_blocks
partial indexes, synced_hashes/block_records/rollup_offsets PKs.

**Unused → pure write tax (DROP):**
- `idx_inodes_updated_at` (both) — **[C]** no `updated_at` query anywhere. + the pg
  `update_inodes_updated_at` trigger that feeds it.
- `idx_inodes_has_acl` (pg only, 000004) — **[C]** no `acl IS NOT NULL` query.
- `idx_parent_child_map_parent` (both) — redundant with PK prefix. **[C]**
- `idx_parent_child_map_parent_name` (pg only) — identical to PK. **[C]**
- `idx_file_block_refs_file_id` (sqlite only) — redundant with PK prefix. **[C]**

**Missing on a possibly-hot read (verify):**
- sqlite `file_block_refs.hash` has **no** index while pg does (000012, dedup live-set).
  If the file-level dedup/audit path queries `file_block_refs` by hash on sqlite it
  seq-scans. **[S]** — low confidence; the primary dedup path is `inodes.object_id`
  (indexed in both). Verify whether any query filters file_block_refs by hash before
  adding.

**Borderline:** `idx_inodes_uid`/`idx_inodes_gid` (partial, file_type=0) exist only for
the **startup** quota-seed `SUM(size),COUNT(*) GROUP BY uid/gid` scan (000033 comment);
they tax every regular-file insert/chown at runtime for a boot-time reader. Keep (the
seed would otherwise full-scan), but note as write cost. `idx_inodes_hidden` partial —
SMB-only, verify a hidden-listing reader exists.

---

## 4. Dead / vestigial

**Confirmed (grep shows no production reader/writer):**
- `pending_writes` table + 2 indexes (pg only). Only ref = truncate list. **DROP.**
- `idx_inodes_updated_at` (both) + `update_inodes_updated_at` trigger (pg). **DROP.**
- `idx_inodes_has_acl` (pg). **DROP.**
- `idx_parent_child_map_parent` (both) + `idx_parent_child_map_parent_name` (pg) —
  redundant with PK. **DROP.**
- `idx_file_block_refs_file_id` (sqlite) — redundant with PK. **DROP.**
- `link_counts` — already dropped (000034 pg; never existed in sqlite). No action.

**Vestigial (journal #1692 obsoletes — do NOT drop on develop yet, cross-ref #1715):**
- `local_chunk_index` table (pg 000036 / sqlite/000003) = badger `li:` twin.
- `rollup_offsets` table (pg 000009 / sqlite/000001) = badger `ro:` twin.
- `block_records` stays until the journal owns block GC (= badger `br:`).

**Suspected / low priority:**
- pg `content_id_hash` column + `inodes_content_id_hash_trigger` — a pg-only md5
  workaround for the btree 2704-byte limit; sqlite indexes `content_id` directly.
  Intrinsic to pg, not removable, but a pg-only per-write cost worth noting.
- `synced_hashes.synced_at` — "preserved for future observability" (000015); no reader
  found. Harmless (in-row).

---

## 5. Ranked hot-path + cleanup candidates

Ordered by effect. "#1715 overlap" flagged where it duplicates the badger cleanup issue
(the block-layer vestigials are shared; the SQL-index items are new).

**H1. Skip `putFileChunkRefs` when `Blocks` is unchanged.** *(NEW — biggest SQL-only win)*
- Today every flush/setattr/chown on a regular file does `DELETE file_block_refs` +
  re-INSERT the full manifest (pg PutFile:476 / sqlite:407 → file_block_refs.go), even
  when only size/mode/mtime changed. **[C]**
- Fix: gate the block-ref rewrite behind a "blocks dirty" flag (or diff against the
  loaded manifest; or split manifest writes into the carve/finalization path and never
  touch them from the attr flush). This mirrors the badger doc's `fb:<uuid>` split (rec 2)
  — SQL already has the side table, it just rewrites it too eagerly.
- Effect: **removes M deletes + M inserts per close/setattr on M-chunk files** — the
  single largest per-op statement + row-churn reduction. Postgres: fewer WAL bytes per
  durable flush. SQLite: fewer serialized `Exec`s on the one writer connection (row-at-a
  -time makes it worse there).
- Risk: **medium** — must be certain the attr path never legitimately changes Blocks
  (truncate does — it prunes to size; keep the rewrite when Size shrinks/`PruneChunkRefsToSize`
  ran). Applies to **both** backends. Overlaps #1715 conceptually (manifest-write
  amplification) but is a distinct SQL code path.

**H2. Drop redundant + dead indexes (write-amp on every namespace/inode write).** *(NEW)*
- Drop `idx_inodes_updated_at` (both) + pg trigger `update_inodes_updated_at`;
  `idx_inodes_has_acl` (pg); `idx_parent_child_map_parent` (both);
  `idx_parent_child_map_parent_name` (pg); `idx_file_block_refs_file_id` (sqlite). **[C]**
- Effect: **each CREATE/MKDIR/RENAME/UNLINK maintains 1–2 fewer btrees; each inode
  UPDATE drops the updated_at btree write AND (pg) the per-row `NOW()` trigger.** Pure
  write reduction, zero read regression (none of these serve a query).
- Risk: **low** — mechanical; one new migration per backend (`000039` pg / `000006`
  sqlite). Not in #1715 (badger has no such indexes).

**H3. Drop the dead `pending_writes` table (pg).** *(NEW)*
- Table + 2 indexes never read/written in production; sqlite never had it. **[C]**
- Effect: removes a dead table (small — it stays empty), tidies the reset/truncate
  loop (snapshot_store.go:56), and deletes an unused FK to `inodes`. Mostly hygiene,
  minor: the empty table costs ~nothing at runtime but the `snapshot_store` truncate
  touches it each reset.
- Risk: **low** (pg-only migration + remove the one Go list entry). Not in #1715.

**H4. [gated on journal #1692] Drop `local_chunk_index` + `rollup_offsets`.** *(overlaps
#1715)*
- SQL twins of badger `li:` / `ro:`. Once the journal `.idx` owns local-read location +
  rollup fence, these tables and their write paths go. **[C]**
- Effect: removes the per-local-chunk `local_chunk_index` upsert and the per-rollup
  `rollup_offsets` upsert from the write path (both backends).
- Risk: **high** correctness coupling — this IS the journal switchover payoff, same
  gating as the badger `li:`/`ro:` deletion. Do not land on develop first. **Cross-ref
  #1715 / #1692 — do not duplicate.**

**H5. [pg, verify] Batch/skip the sqlite row-at-a-time block-ref insert.** *(NEW, folds
into H1)*
- If H1 lands, the M-Exec sqlite loop (file_block_refs.go:41) mostly disappears for the
  attr path. For the genuine carve path, sqlite has no batch protocol, but a single
  multi-row `INSERT ... VALUES (...),(...),...` (built with placeholders) replaces M
  round-trips with one statement on the single writer.
- Effect: sqlite carve write becomes one statement instead of M. Risk: low (statement
  construction). SQLite-only. Subsumed by H1 for the non-carve paths.

**Not applicable / refuted:**
- GORM footguns (bool default:true, OnConflict reload, UpdateShare map): **N/A** — these
  backends are raw pgx/database/sql, not GORM. **[C]**
- "One fsync per commit" reduction (the badger lever): **already done** — sqlite
  WAL+NORMAL and pg RelaxedDurability. No further per-commit-fsync win available at the
  SQL layer; the remaining SQL cost is statements/rows/indexes (H1–H3), not fsync count.
- Merging CREATE+WRITE+CLOSE into one txn: same constraint as badger — distinct NFS RPCs,
  cannot collapse at the store layer.

---

## Postgres-vs-SQLite divergence summary

| Aspect | Postgres | SQLite | Note |
|---|---|---|---|
| Migrations | 38 incremental | 5 (000001 = consolidated final state) | sqlite has no legacy installs |
| Per-commit fsync | on (relaxable via synchronous_commit=off) | **off** (WAL+NORMAL, checkpoint-only) | sqlite already dodges the fsync wall |
| Concurrency | pool MaxConns=10 | **single writer (MaxOpenConns=1)** | sqlite's real wall |
| updated_at on UPDATE | trigger recomputes NOW() | not maintained | pg does extra work |
| content_id index | md5 hash column + trigger | direct column | pg btree-limit workaround |
| block-ref insert | pgx.Batch (1 round-trip) | M separate Exec | sqlite worse pre-H1 |
| Dead `pending_writes` table | present | absent | drop from pg |
| `idx_inodes_has_acl` | present (dead) | absent | drop from pg |
| `idx_parent_child_map_parent_name` | present (=PK) | absent | drop from pg |
| `idx_file_block_refs_file_id` | absent (PK covers) | present (redundant) | drop from sqlite |
| `idx_file_block_refs_hash` | present | **absent** | verify sqlite dedup path |
| block_layout, object_id, eas, acl | columns | columns | parity (in-row) |

The two backends have drifted: **sqlite carries redundant/missing indexes vs pg**
(`idx_file_block_refs_file_id` extra, `idx_file_block_refs_hash` missing), and **pg
carries dead weight sqlite avoids** (`pending_writes`, `idx_inodes_has_acl`,
`idx_parent_child_map_parent_name`, updated_at trigger). H2/H3 realign them.
