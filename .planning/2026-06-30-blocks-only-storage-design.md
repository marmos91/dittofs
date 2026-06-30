# DittoFS Blocks-Only Storage — Design

**Status:** approved design (brainstorm complete) — basis for implementation planning.
**Tracking epic:** #1493. **Origin:** #1414.
**Supersedes:** the additive "object packing PR3b" blueprint
(`.planning/2026-06-30-pr3b-packer-blueprint.md`) and the PR3a/PR3b two-object-kind model.
**Implementation:** on a **separate branch off `develop`**; the in-flight `1414-pr3b`
additive commits are discarded and re-implemented against this design.

## Principle

**Only the data-at-rest representation changes** — how bytes are saved on local disk
and in the remote store. The **metadata model is untouched**: every chunk is still
referenced by its BLAKE3 content hash, and dedup + refcounting stay keyed by that hash.

The remote store no longer holds one object per chunk (`cas/<hash>`). Instead, chunks
are packed into **blocks**, and the remote store holds **only blocks**. A block is a
self-describing container of one or more chunks; the store does not know or care how
many chunks a block holds.

**Local disk holds only raw log blobs — never block files.** A block is assembled
*streaming at upload time*, carved in real time from a log blob, and exists only on the
wire and in the remote store. This removes the write-amplification (no log→block copy) and
the double on-disk storage (no separate block file), and keeps the write path a pure raw
append. This is the central performance win of the design.

## Vocabulary

- **Chunk** — a FastCDC-cut, content-addressed (BLAKE3) unit. The dedup unit. Lives
  *inside* a block (remotely); locally it lives as a byte range of a raw log blob.
- **Log blob** — a large, bounded, append-only file of **raw** (untransformed) bytes —
  the only data artifact on local disk. Sized for efficient sequential writes / fsync
  amortization (default cap ~1 GB). The "append log" is a **sequence** of log blobs; the
  current one receives appends, older ones are read / evicted. Rotation is deliberate:
  bounded files keep on-disk objects manageable **and contain corruption blast radius** —
  a damaged blob loses at most its own not-yet-evicted chunks, not the whole store.
- **Block** — the unit of remote transfer/storage: `[preamble][record]…` where each
  record carries one chunk. A block holds 1..N chunks, bounded to ~16 MiB (S3 PUT sweet
  spot). Built streaming at upload; **never written to local disk**. **One log blob yields
  MANY blocks** — a ~1 GB blob carves into ~64 blocks. A chunk never spans blocks (a chunk
  larger than the block target becomes a one-chunk block).
- **Local index** — the local store's private map `chunk hash → {logBlobID, rawOffset,
  rawLength}`. Serves local reads directly from the raw log blob (zero decode).
- **Remote locator** — the metadata `SyncedHashStore` map `chunk hash → {BlockID,
  wireOffset, wireLength}`, recorded after upload. Resolves a chunk inside the remote
  block. (Local raw offsets ≠ remote wire offsets — records are interleaved and bodies
  transformed — so the two indexes are intentionally separate, as they already are today.)
- **Block record** — a metadata row per block: `BlockID → {blockHash, length,
  liveChunkCount, syncState}`. `blockHash` (BLAKE3 of the block bytes) is the authoritative
  whole-object integrity check, kept *away from the data* (ZFS-style checksum-in-parent);
  `liveChunkCount` drives delete-only GC without scanning.

## Data path

The local store works like a RocksDB-style block-structured file: one large log blob,
**written at the tail and read at earlier offsets concurrently** (positioned `pwrite` /
`pread`). Blocks are **cut out of the blob in real time** as enough contiguous chunks
accumulate — we never wait to "seal" a whole 1 GB blob before uploading.

```
NFS/SMB write ─▶ pwrite RAW bytes at the TAIL of the current LOG BLOB (large file)  [max speed]
       │  durable on NFS commit / SMB close (fsync)
       ▼
  FastCDC cuts chunks over the newly-written raw region
       │  dedup: hash already known? ─▶ refcount bump, do NOT re-store
       │  else ─▶ local index: hash → {logBlobID, rawOffset, rawLength}  (state: local)
       ▼
  ~BlockSize of contiguous chunks accumulated (or idle) ─▶ CARVE a BLOCK in real time:
       │  pread that offset range from the blob (concurrent with ongoing tail appends),
       │  emit [preamble][record {hash,length}(AEAD on enc shares) + enc(comp(chunk))…],
       │  stream ─▶ RemoteBlockStore.PutBlock(blockID)        (never written to local disk)
       │  ─▶ SyncedHashStore records hash → {BlockID, wireOffset, wireLength}; mark synced
       ▼
  log blob hits its size cap (~1 GB) or idle ─▶ start a new log blob
       │  fully-synced log blobs are deleted under cache pressure (eviction)
```

A **partial log blob on disk is a normal, durable state.** A write returns once its raw
bytes are in the (fsynced) log blob and the chunk is indexed; block carving + upload
happen lazily and concurrently with ongoing appends. The block is materialized only on
the wire — never as a local file.

## Block format

```
Block object (remote; built streaming at upload — never a local file):
  [ preamble ][ record_0 ][ record_1 ] … [ record_{N-1} ]

preamble = magic + version + BlockID                (emitted first as the block is carved)
record_i = { hash, length }  +  chunk wire bytes
           └── inline index ──┘    └─ enc(comp(chunk_i)) ─┘
```

- **Inline per-chunk records** form a distributed, self-describing index emitted in a
  single streaming pass as the block is carved from the log blob — **no second pass and no
  footer rewrite** (the index is interleaved with the bodies as we go).
- **Chunk wire bytes** are independently transformed (`enc(comp(chunk))`) so each chunk
  is independently decodable — required for byte-range reads and per-chunk dedup. This is
  the offset-consistency contract: the locator `Offset/Length` are over the **wire
  bytes as stored**, not plaintext.
- **Encryption:** on an encrypted share, both the chunk wire bytes and each record's
  `{hash,length}` sub-header are AEAD-framed (so the block leaks no content
  identifiers). On a non-encrypted share the records and bodies are plaintext.
- **Whole-block integrity** lives in the metadata **block record** (`blockHash` = BLAKE3 of
  the block bytes), not inside the block — verified on fetch (`blake3(block)==blockHash`) to
  catch truncation / garbling / misdirected fetch before any decode. The preamble holds only
  self-ID (magic/version/BlockID) so DR can still rebuild the chunk index from blocks alone.
- **Range read** uses the metadata locator (`Offset/Length`) directly — it does not need
  to parse records. **Disaster recovery** scans the records sequentially to rebuild the
  chunk index (`hash → {thisBlock, offset, length}`); on an encrypted share DR needs the
  key (the data is encrypted anyway).
- **Header / metadata recovery scope:** the block restores the **chunk index only**.
  Full file-namespace recovery (which file is composed of which chunks) is **out of
  scope** here and will be addressed later via remote **manifest files** (deferred issue).

## Components

1. **Remote block store interface (slimmed).**
   - Writes: `PutBlock(ctx, blockID, data)` — the *only* writer.
   - Reads: `GetBlock(ctx, blockID)` (whole) + `GetBlockRange(ctx, blockID, off, len)`.
   - Lifecycle/GC: `DeleteBlock(ctx, blockID)`, `Walk(ctx, blocks/…)`.
   - **Removed:** `Put/Get/GetRange`-by-hash and the `cas/{hh}/{hh}/<hex>` key shape.
   - The store is dumb: it stores opaque block objects and serves whole/range reads.

2. **Block codec.** Builds `[preamble][record]…`, computes locators, parses records for
   DR. Per-chunk transform reuses the existing compression/encryption decorators (each
   chunk independently framed); record sub-headers AEAD-framed on encrypted shares.

3. **Local tier.** The only on-disk data artifact is the sequence of raw **log blobs**
   (RocksDB-style: append at the tail, read earlier offsets concurrently). No block files,
   no per-chunk CAS files. The local store keeps its own index `hash → {logBlobID,
   rawOffset, rawLength}` and serves local reads straight from the blob (zero decode).
   Eviction drops **whole synced log blobs** to reclaim disk; on a later read of an evicted
   chunk, GET its (whole) remote block, decode, verify, and re-stage into a fresh log blob.

4. **Metadata.** Per-chunk surface unchanged: `chunk hash → {BlockID, Offset, Length}`;
   dedup short-circuit before a chunk is stored (known hash → refcount bump only); refcount
   cascade on unlink. **Added: a per-block record** `BlockID → {blockHash, length,
   liveChunkCount, syncState}` — `blockHash` for whole-object integrity, `liveChunkCount`
   for GC. All chunk locators + the block record for one carved block are written in a
   **single transaction** at upload. This batched-per-block write is the store-contract
   batched-commit capability of **#1416** — one fsync per block, not per file — which
   removes the per-file fsync wall.

5. **Sync engine.** Carves blocks in real time (block-size or idle), `pread`s the carved
   offset range from the log blob, streams it transformed+framed into `PutBlock`, and
   writes all of that block's chunk locators atomically. Crash after `PutBlock` but before
   the locator write → those chunks stay unsynced and are re-carved into a new block on the
   next pass (re-upload is idempotent); the orphan block is reclaimed by GC.

6. **GC (delete-only).** The block record's `liveChunkCount` is decremented as chunks reach
   refcount 0; at 0 the block is deleted (local + remote) and its record removed — no scan.
   Compaction of partially-dead blocks is deferred (see below).

7. **Migration.** Automatic, **blocking at startup**, resumable: detect `cas/<hash>`
   objects (local + remote), re-pack them into blocks, rewrite their locators in one
   pass, delete the old objects. The legacy standalone reader exists **only** inside this
   routine; once migration completes, the serving path is blocks-only. A half-migrated
   store re-runs the pass cleanly (idempotent).

8. **Concurrency.** One active log blob per share, mutex-serialized tail appends;
   concurrent files interleave their bytes/chunks in the blob — fine, since chunks are
   content-addressed and locators map each independently of file composition. Block carving
   (`pread` of completed regions + upload) runs concurrently with ongoing tail appends.

### Defaults (tunable)

- **TargetBlockSize:** 16 MiB (S3 PUT sweet spot; a single chunk larger than this becomes
  a one-chunk block).
- **Block-carve idle timeout:** 5s. Carve+upload a sub-`BlockSize` block when no new
  chunks arrive. Gates only **upload latency**, not durability — a chunk is durable in the
  (fsynced) log blob the instant it's written, well before carve.
- **LogBlobSizeCap:** ~1 GB (rotate to a new blob at the cap, or on long idle). Bounds
  file size and corruption blast radius.

## Performance & footprint requirements (now)

These are part of the design, not optimizations — they keep the footprint bounded
without added complexity:

- **No local block materialization.** Blocks are built streaming at upload by `pread`ing
  the log blob and transforming+framing on the fly into `PutBlock` (`io.Reader`); a whole
  block is never held in RAM or written to local disk. RAM is bounded to one chunk +
  transform buffers via `sync.Pool`.
- **Single-pass write path.** The hot path is a raw `pwrite` append to the log blob — no
  transform, no copy. FastCDC + hashing index the written region; transform happens once,
  later, at upload.
- Steady-state on-disk cost: **1 raw write** (log blob append). The only extra read is the
  `pread` at upload (unavoidable — bytes must reach the network). No log→block copy, no
  second on-disk artifact.

## Error handling

- Every range/whole read verifies `blake3(plaintext) == hash` after decode — fail-closed
  (bad bytes never reach the caller), as today.
- `PutBlock` is idempotent on `blockID`; the locator write is idempotent (re-pack safe).
- Migration is resumable / idempotent.

## Corruption handling

Corruption is a first-class concern, detected and recovered rather than propagated.

- **Detection (end-to-end).** Content-addressing makes corruption self-detecting: every
  read verifies `blake3(plaintext) == hash` and **fails closed** (bad bytes never reach the
  caller). The inline record framing (`{hash,length}` per chunk) lets a torn/garbled block
  be caught structurally before decode.
- **Whole-block integrity (away from data).** The metadata block record's `blockHash` is
  verified on fetch (`blake3(block) == blockHash`) — a ZFS-style checksum-in-parent that
  catches truncation, garbling, and **misdirected / lost writes** a co-located checksum
  would miss, cheaply and before any decode.
- **Containment.** Bounded log blobs (~1 GB) cap the blast radius — a damaged blob risks at
  most its own not-yet-evicted chunks, never the whole store.
- **Local recovery (cache model).** The local tier is a cache; the remote block is the
  durable copy. A local hash-mismatch (bitrot, torn write) → refetch the chunk's block from
  remote, re-verify, re-stage. No data loss as long as the chunk was synced.
- **Crash / torn-tail recovery.** On restart, the active log blob's tail (bytes after the
  last fsync'd commit/close boundary) may be torn. The durable local index is the source of
  truth for which chunks are valid; the blob is truncated to the last validated chunk.
  NFS-commit / SMB-close fsync boundaries bound what can be lost (only un-acked writes).
- **Remote corruption.** Detected via `blake3` on read (fail-closed). If a local copy still
  exists, re-upload; otherwise surface a data-loss error with a metric/alert. A periodic
  background **scrubber** (proactive re-verification of blobs/blocks) is deferred.

## Testing

- Block codec round-trip: build → record parse → range read → blake3 verify; including
  encrypted shares and incompressible/random input (adaptive compression fallback).
- Dedup: identical chunk across files → one block record, refcount 2.
- Carve triggers (block-size, idle); shutdown carves + uploads the pending tail region.
- GC: block deleted only when all its chunks hit refcount 0.
- Migration: `cas/<hash>` fixture → blocks-only, locators rewritten, old objects gone,
  data byte-identical; resumable mid-run.
- Eviction → whole-block refetch round-trip.
- Crash window: `PutBlock` ok, locator write fails → chunks re-carved, no data loss.
- Corruption: flip a byte in a local log blob → read detects mismatch and recovers from
  the remote block; flip a byte in a remote block → read fails closed.
- Torn tail: truncated/garbled active log blob → restart truncates to the last validated
  chunk; acked (committed) data survives, only un-acked tail is dropped.

## Cleanup of pre-existing code (required — part of definition of done)

This redesign **replaces** the prior storage path; the implementation must **delete** the
superseded code, not layer on top of it (no compat shims — pre-1.0, and migration handles
existing data). Leftover standalone-path code is a bug, not acceptable dead code.

- **Remove the `cas/<hash>` standalone object path** end to end: `RemoteStore.Put/Get/
  GetRange`-by-hash, `FormatCASKey`/`ParseCASKey`, the `cas/` walk + keying, and
  `ReadBlockVerified`'s standalone use.
- **Remove the additive object-packing scaffolding** (abandoned PR3a/PR3b): the
  standalone-vs-block `ChunkLocator` branch in the read path, the `BlockWriter` /
  `ChunkTransformer` / `ReadChunk` decorator surface **as built for the additive model**,
  and the PR3b packer blueprint artifacts. Re-derive only what the blocks-only design needs.
- **Remove per-chunk local CAS file storage** in `pkg/block/local/fs`, replaced by log blobs.
- **Retire the old rollup → standalone-mirror sync path** (`mirrorChunk` standalone `Put` +
  standalone `MarkSynced`), replaced by block carving + `PutBlock`.
- **Discard the in-flight `1414-pr3b` commits** (additive `BlockWriter`/`ChunkTransformer`/
  `MarkSyncedBatch`) — re-implement against this design on the new branch.
- Grep for and delete now-dead helpers, tests, and config knobs tied to the standalone
  path; leave no vestigial interfaces (per the minimize-interface-surface rule).

## Deferred — future work (tracked as issues)

- **GC compaction (B), near-term — #1487.** Rewrite partially-dead blocks (copy live chunks
  into a fresh block, rewrite locators, delete old). Future: do it server-side via S3
  `UploadPartCopy` over live byte-ranges (no client bandwidth).
- **Chunk-range refetch (eviction B) — #1488.** On a read miss, GET only the needed chunk
  range and cache it by hash, instead of refetching the whole block. Lower random-read
  footprint; local tier then holds whole blocks + per-chunk cache entries.
- **Remote manifest files — #1489** for full file-namespace disaster recovery (beyond the
  chunk index the block headers already provide).
- **Background corruption scrubber — #1490** — periodic proactive re-verification of local
  log blobs and remote blocks (re-hash sampled chunks), repairing local damage from the
  remote copy and alerting on remote damage, instead of only detecting on read.
- **Decoupled log-blob / block sizing — #1491** — currently a log blob caps at ~1 GB and blocks are
  carved at ~16 MiB; revisit if the two need independent tuning per workload.
- **Group-commit fsync — #1492** on log-blob appends to amortize durability syscalls under
  many small writes.

(The earlier "zero-copy log→block" and "log-is-the-block fusion" ideas are now **part of
the core design** — there is no separate local block file to copy into.)

## Notes on existing in-flight work

The worktree commits 1–2 (`BlockWriter` / `ChunkTransformer` interfaces, S3/memory
`PutBlock`, decorator pass-through + per-chunk transform, `nonClosingRemote` delegation)
**partially align** with this design (remote `PutBlock`, per-chunk encrypted wire bytes,
transform reuse). They will be triaged keep-vs-rework during planning — notably the
remote interface here *removes* `Put`-by-hash entirely, which those commits still assume.
