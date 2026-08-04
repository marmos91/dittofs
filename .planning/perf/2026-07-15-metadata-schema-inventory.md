# Badger Metadata Store — Schema / Entity Inventory (for #1687 schema cleanup)

Read-only investigation, branch `develop`, 2026-07-15. Goal: reduce what the
badger default metadata store **persists and fsyncs** to relieve the #1687
throughput wall (`db.lock` RLock held across per-COMMIT WAL fsync vs per-write
`Lock` in `ensureRoomForWrite`). The lever ranked highest is **fewer `db.Sync`
calls per file op**; secondarily **smaller per-commit WAL payload**.

Durability model (badger/store.go:282-313, 476-490):
- **Strict (default, `RelaxedDurability=false` → `SyncWrites=true`)**: EVERY badger
  commit (`db.Update`, incl. every `WithTransaction` and every bare `s.db.Update`)
  fsyncs the WAL. This is where #1687 bites hardest — every key write below that
  goes through its own `db.Update` is a `db.Sync`.
- **Relaxed (`RelaxedDurability=true`, #1573 Wall 1)**: `SyncWrites=false`; only
  paths that explicitly call `syncIfRelaxed()` fsync inline — that is
  `WithTransaction` (durable=true) and `SetRollupOffset`. Namespace/attr ops go
  through `withRelaxedTransaction` → `WithTransactionRelaxed` and defer to a 1 s
  background ticker (`runDurabilitySync`). Bare `s.db.Update` writes (block record,
  local-chunk index, durable handles) are NOT synced inline in relaxed mode.

`deferredCommit` defaults to **true** (metadata/service.go:151): per-WRITE RPCs
accumulate in RAM (`pendingWrites`) and are flushed by ONE durable
`WithTransaction` on NFS COMMIT/close. READ does **not** persist atime (no
atime-on-read write anywhere).

---

## Storage map (all persisted key prefixes)

| Entity | Prefix | Key format | Value | Written on | Hot-path durable commit? | Verdict |
|---|---|---|---|---|---|---|
| File inode | `f:` | `f:<uuid>` | File JSON (whole `FileAttr` incl ACL, EAs, Blocks, ObjectID, recycle fields) | create, write-flush, setattr, chown, rename, unlink, recycle | YES — create (relaxed), flush (durable), setattr | KEEP (but REDUCIBLE, see below) |
| Parent edge | `p:` | `p:<childUUID>` | parentUUID (16 B) | create, rename | rides same txn | KEEP |
| Child edge | `c:` | `c:<parentUUID>:<name>` | childUUID (16 B) | create, rename, unlink | rides same txn | KEEP |
| Child-name reverse edge | `cn:` | `cn:<parentUUID>:<childUUID>` | name | create, rename, unlink | rides same txn | KEEP (derivePath O(1), #1166) |
| Link count | `l:` | `l:<uuid>` | uint32 BE | create, link, unlink, mkdir(parent) | rides same txn | KEEP |
| Device number | `d:` | `d:<uuid>` | deviceNumber JSON | **never** | — | **DEAD-confirmed** |
| Share | `s:` | `s:<shareName>` | shareData JSON (Share + RootHandle) | AddShare/UpdateShare | rare (control-plane) | KEEP |
| Server config | `cfg:` | `cfg:server` | MetadataServerConfig JSON | startup/config | rare | KEEP |
| FS capabilities | `cap:` | `cap:fs` | FilesystemCapabilities JSON | startup | rare | KEEP |
| FS meta | `fsmeta:` | `fsmeta:<share>` | FilesystemMeta JSON | statfs writeback / rare | rare | KEEP |
| ObjectID index | `obj:` | `obj:<hex>` | fileUUID (16 B) | PutFile *iff ObjectID changed* | quiesce/flush (not size-bump) | KEEP |
| PayloadID index | `pl:` | `pl:<payloadID>` | fileUUID (16 B) | PutFile *iff PayloadID new* | create only | KEEP |
| Block record | `br:` | `br:<blockID>` | block.BlockRecord JSON | CommitBlock, GC decr | sync/GC path (bare Update) | KEEP (engine GC) |
| Local chunk index | `li:` | `li:<hex(hash)>` | block.LocalChunkLocation JSON | every local chunk store (bare Update) | **YES in strict — 1 sync/chunk** | **VESTIGIAL-after-journal** |
| Rollup offset | `ro:` | `ro:<payloadID>` | uint64 LE (8 B) | rollup/compaction (`SetRollupOffset`) | **YES always — `syncIfRelaxed` forces fsync even in relaxed** | **VESTIGIAL-after-journal** |
| Synced-hash marker | `synced:` | `synced:<hash>` | nanos(8B)+ChunkLocator | MarkSynced/DeleteSynced (carve/sync) | sync/GC path | KEEP (engine sync + GC) |
| NLM lock | `lock:` `lkfile:` `lkowner:` `lkclient:` `srvepoch` | see locks.go:23-27 | lock JSON / index / epoch | lock/unlock | lock ops | KEEP (owned by other session) |
| NSM client | `nsm:client:` `nsm:monname:` | clients.go:21-26 | client JSON | NSM notify/register | rare | KEEP |
| SMB durable handle | `dh:id:` `dh:cguid:` `dh:appid:` `dh:fid:` `dh:share:` | durable_handles.go:18-33 | handle JSON / index | SMB open/close (bare Update) | SMB path only (not NFS hot path) | KEEP |
| Quota usage | — | — | in-memory `userUsage`/`groupUsage`, rebuilt from `f:` rows at startup (store.go:533-587) | — | **not persisted** | KEEP (nothing to fsync) |
| Snapshot/backup | — | — | `WriteSnapshot`/`ReadSnapshot` length-prefixed stream (snapshot_store.go) | backup/restore only | not a live keyspace | KEEP |

Confirmed-dead vs suspected:
- **DEAD-confirmed**: `d:` device-number prefix — the only two repo-wide hits are
  the doc comment (encoding.go:40) and the const (encoding.go:51). No
  `keyDeviceNumber`, no setter, no getter. Device numbers live in `File.Rdev`
  (file_types.go:67) inside the `f:` blob.
- **VESTIGIAL-verify (gated on block-layer switchover)**: `li:` and `ro:` —
  consumed ONLY by `pkg/block/local/fs/` (the legacy log-blob local store the
  journal redesign replaces). See "Block-layer coupling" below. A parallel
  session is mid-switchover; do not delete yet, but they are the largest hot-path
  fsync reducers once the journal's own `.idx` becomes authoritative.

---

## Entity detail

### File inode — `f:<uuid>` → File JSON  (the hot entity)

The whole `File`/`FileAttr` struct is JSON-serialized into ONE badger value and
re-serialized in full on every mutation (encoding.go:150-165,
transaction.go:430-437). Fields (file_types.go:13-140):

| Field | Type | Purpose | Written on | Read by | Verdict |
|---|---|---|---|---|---|
| ID | uuid | identity | create | everywhere | KEEP |
| ShareName | string | share routing | create | handle routing | KEEP |
| Path | string | display path | **zeroed before encode** (encoding.go:150-159) | derived via `derivePath` on read | KEEP (never persisted) |
| Type | FileType | reg/dir/symlink/dev | create | all | KEEP |
| Mode | uint32 | perm bits | create, setattr, SUID-clear | perm checks | KEEP |
| UID/GID | uint32 | owner | create, chown | perm/quota | KEEP |
| Nlink | uint32 | link count | **also mirrored in `l:` key** | GetFile overrides from `l:` (files.go:151-168) | REDUCIBLE (dup of `l:`) |
| Size | uint64 | size | **write-flush**, setattr | read/getattr | KEEP (hot) |
| Atime | time | access time | create, setattr | getattr | REDUCIBLE (not bumped on read; only setattr) |
| Mtime/Ctime | time | mod/change time | **write-flush**, create, setattr | getattr | KEEP (hot) |
| CreationTime | time | birth time | create | getattr | KEEP |
| PayloadID | string | content id | create | rollup/read | KEEP (indexed by `pl:`) |
| LinkTarget | string | symlink target | create(symlink) | readlink | KEEP |
| Rdev | uint64 | device major/minor | create(dev) | getattr | KEEP (this is why `d:` is dead) |
| Hidden | bool | SMB hidden | setattr | listing | KEEP |
| ACL | *acl.ACL | NFSv4 ACL | setattr | perm check | KEEP (rides blob) |
| EAs | map[string][]byte | SMB xattr/EA | SetXattr (RMW of File) | getxattr | KEEP (rides blob; see note) |
| IdempotencyToken | uint64 | dup-create detect | create | create | KEEP-verify (low use) |
| Blocks | []ChunkRef | content chunk manifest | sync finalization | read/carve | KEEP but **heavy** — see REDUCIBLE-2 |
| ObjectID | ObjectID | Merkle root, dedup | quiesce | FindByObjectID (`obj:`) | KEEP |
| DeletedAt/OriginalPath/DeletedBy | recycle fields | trash | recycle/restore | trash listing | KEEP |

Note (xattr): there is NO separate xattr keyspace — `SetXattr` /
`ResolveSetXattr` read-modify-write the whole `File` blob (badger/xattr.go +
metadata/xattr resolve). Every EA set re-serializes the entire inode.

### Secondary indexes maintained inside PutFile (transaction.go:440-475+)
- `obj:<hex>` written/deleted only when `File.ObjectID` changes (delete stale +
  probe-for-conflict + set). On a hot size-bump flush ObjectID is unchanged →
  the obj-index is a READ (probe), not a write.
- `pl:<payloadID>` written when PayloadID is new (create). Not rewritten on flush.
So on the hot write-flush the commit writes essentially just the `f:` blob (full
re-serialization) — index churn is not the cost; the blob size is.

### Block-layer coupling (li: / ro:) — the biggest hot-path fsync reducers, but gated
- `li:` `PutLocalLocation` (block_record_store.go:386-400) is a bare `s.db.Update`
  → in **strict** mode it is a `db.Sync` on **every unique local chunk stored**.
  Consumed only by `pkg/block/local/fs/{chunkstore,eviction,blockstore_methods,
  legacy_migration}.go`.
- `ro:` `SetRollupOffset` (rollup.go:56-119) calls `syncIfRelaxed()` explicitly →
  a **forced fsync per rollup even in relaxed mode**. Consumed only by
  `pkg/block/local/fs/{appendlog,appendwrite,compaction,recovery}.go`.
- The journal redesign (`pkg/block/journal/`, memory index `project_walla…`)
  makes the journal's own interval/`.idx` authoritative for LOCAL reads, which
  removes the need for `li:` and the `ro:` fence. When that lands, deleting the
  `li:`/`ro:` keyspaces removes 1 sync/chunk (strict) + 1 forced sync/rollup
  (both modes) from the metadata DB. **Do not delete on develop yet** — the
  local `fs` store still routes chunk persistence through them.

---

## Simplification candidates (ranked by effect on db.Sync / write volume)

1. **[gated on journal] Delete `li:` LocalChunkIndex + `ro:` rollup-offset keyspaces.**
   Change: after journal switchover, drop `PutLocalLocation`/`GetLocalLocation`/
   `WalkLocalLocations` (block_record_store.go:197-521) and all of rollup.go +
   `prefixLocalChunkIdx`/`rollupOffsetPrefix`. Effect: **removes 1 db.Sync per
   local chunk (strict) and 1 forced fsync per rollup (both modes)** — the single
   biggest sync-frequency win on the write path. Risk: high correctness coupling —
   requires the journal to own local-read location + rollup fence first.
   Independent of block switchover? **NO — this IS the switchover's payoff.**

2. **[independent] Reduce per-commit WAL payload: split `Blocks[]` (and optionally
   ACL/EAs) out of the `f:` blob.** Change: the hot write-flush (io.go:434-457,
   PutFile transaction.go:430) re-serializes the ENTIRE File JSON — including the
   whole `Blocks []ChunkRef` manifest — just to bump size+mtime+ctime. Store the
   block manifest under a sibling key (e.g. `fb:<uuid>`) written only at sync
   finalization, so an attr-only flush writes a small blob. Effect: **smaller WAL
   fsync payload per hot commit**, and a smaller commit is less likely to trip
   `ensureRoomForWrite`. Risk: medium — touches encoding + every File reader; needs
   a read shim / migration. Independent of block switchover? **YES.**

3. **[independent] Fold the SUID/SGID-clear write into the deferred flush.**
   Change: `deferredCommitWrite` (io.go:245-257) fires a SEPARATE fully-durable
   `WithTransaction` to clear SUID/SGID before recording the pending write; the
   later `flushPendingWrite` re-writes the same `f:` row and already honors
   `state.ClearSetuidSetgid` (io.go:452-454). Drop the eager txn and let the flush
   carry it, provided the pending-state GetFile merge exposes the cleared MODE to a
   piggybacked GETATTR (verify the merge covers MODE — the comment at io.go:234-238
   is why it was made eager). Effect: **−1 db.Sync on the (rare) SUID-file write**.
   Risk: low-medium (GETATTR mode-visibility). Independent? **YES.**

4. **[independent, trivial] Delete the dead `d:` device-number prefix.**
   Change: remove encoding.go:40 (comment) + :51 (`prefixDeviceNumber`). Effect:
   none on runtime (never written) — pure dead-code removal. Risk: none.
   Independent? **YES.**

5. **[independent, small] Drop `Nlink` from the persisted `f:` blob.** Change:
   `Nlink` is authoritative in the `l:<uuid>` key and GetFile already overrides the
   blob value from `l:` (files.go:151-168). Persisting it in the JSON too is
   redundant write volume and a potential drift source. Encode it as `-` / omit.
   Effect: marginally smaller blob; removes a dead field. Risk: low (grep for any
   reader that trusts `File.Nlink` without the `l:` override). Independent? **YES.**

6. **[independent, verify] Stop persisting `Atime`, or make it lazy.** Change:
   atime is written on create/setattr but is NOT bumped on read (confirmed: no
   atime-on-read write). It is low-value POSIX metadata. If any client relies on it
   it must stay, but it could be dropped from the hot flush path (the flush at
   io.go:448-451 already only touches mtime/ctime, so atime is not currently a hot
   cost — this is a write-volume nicety, not a sync-count win). Effect: negligible
   on sync count. Risk: low. Independent? **YES.** (Listed for completeness; low
   priority.)

Not recommended (out of scope / refuted):
- Cross-thread commit coalescing / group-commit — refuted ×3.
- badger vlog/memtable tuning — eliminated (values are sub-ValueThreshold).
- Merging create + write-flush into one txn — they are distinct NFS RPCs
  (CREATE, then WRITE, then COMMIT); cannot be collapsed at the store layer.

---

## Hot-path performance: what the journal (#1692) unlocks

The refuted attacks all tried to make the metadata WAL fsync *cheaper or fewer*.
The journal enables a structurally different move: **relocate the data-durability
fsync off the badger WAL entirely**, onto the journal's own sequential append fd
(`pkg/block/journal/segment.go`), which has no `db.lock`. That is precisely the
resource #1687 contends on (`db.lock` RLock held across the per-commit WAL fsync).
Grounding: the journal already provides fsync-durable checkpoints (journal/doc.go:3)
and an index mapping `FileID + FileOffset + Length` (journal/carve.go:51,
journal/index.go:16-17), so file size is reconstructable on recovery by scanning
its records; the synced bit is flipped with NO fsync (journal/carve.go:308, "a lost
flip just re-carves, dedup makes it idempotent"). This is NOT the refuted
cross-thread coalescing — it moves the fsync, it does not batch it.

Three levers, ranked by effect on metadata `db.Sync` frequency:

7. **[the design change — must land WITH the switchover] Make the CLOSE flush
   relaxed once the journal owns data durability.** `flushPendingWrite` (io.go:434)
   is the one forced `WithTransaction` (= db.Sync) per file close, forced ONLY
   because size is data-paired (#588: lose size → read past EOF returns zeros). With
   the journal fsyncing the data and its index yielding authoritative size on
   recovery, that constraint dissolves: order it journal-fsync (data durable) →
   metadata **relaxed** commit (deferred to the 1 s ticker); a crash in between is
   recovered by deriving size from the journal index. Effect: **removes the last
   per-file-op forced metadata fsync on the write path** — after this, steady-state
   create→write→close→carve issues ZERO forced metadata fsyncs and the #1687 lock
   stops being held across a sync during writes. Risk: HIGH — ordering discipline;
   requires journal recovery (journal/recovery.go) to reduce to a per-file
   `size = max(fileOffset+len)`, and WCC/GETATTR to keep reading RAM pending-state
   (they already do — pending is ahead of the durable commit). Independent? **NO —
   flip flush to `withRelaxedTransaction` the moment the journal becomes the
   durability source.** This is the actual dissolution of the wall, not a mitigation.

   (Levers 1 [`li:`+`ro:` deletion] and 2/3 above compound with this: once flush is
   relaxed AND `li:`/`ro:` are gone, the metadata store does no forced fsyncs at all
   on the steady-state write path.)

What the journal does NOT fix:
- **CREATE stays a metadata commit** (relaxed WAL append + eventual ticker fsync) —
  the journal has no bearing on pure namespace/dirent writes. If create throughput
  is still a wall after lever 7, the remedy is the `f:`-blob-size trim (rec 2, split
  `Blocks[]`), which is independent of the journal.
- **Ordering is the whole risk of lever 7.** Verify journal/recovery.go reconstructs
  `File.Size` before implementing. (This audit was read-only; recovery.go's size
  reconstruction was NOT verified.)
