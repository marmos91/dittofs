# Blocks-Only Storage — Implementation Plan

**Design:** `.planning/2026-06-30-blocks-only-storage-design.md`
**Epic:** #1493 · **Origin:** #1414 · **Subsumes:** #1416 (batched metadata commits, in P3)
**Branch:** implement on a **new branch off `develop`** (e.g. `blocks-only-storage`). The
in-flight `1414-pr3b` additive commits are **discarded** and re-implemented against this
design. Each phase below is a shippable PR; cleanup of the superseded path is woven into the
phases it touches (it is part of each phase's definition of done, not a separate cleanup PR).

## Dependency order

```
P1 codec ─┐
P2 remote ┼─▶ P5 sync/read ─▶ P6 GC ─▶ P7 migration ─▶ P8 corruption hardening
P3 meta  ─┤        ▲
P4 local ─┘────────┘
```

## Phases

### P1 — Block codec (pure, isolated)
- New package: block builder + reader. Format `[preamble: magic|version|BlockID][record:
  {hash,length} + chunk wire `enc(comp(chunk))`]…`; inline self-describing index.
- Streaming builder (carve from a byte source, one pass, emit records inline) and reader
  (whole + byte-range); compute `blockHash` (BLAKE3 of block bytes).
- Reuse the existing compression/encryption decorators for per-chunk wire bytes; AEAD-frame
  records + bodies on encrypted shares; plaintext otherwise.
- **DoD/tests:** build→parse→range-read→`blake3==hash`; encrypted + incompressible inputs;
  DR scan rebuilds the chunk index from a block alone.

### P2 — Remote block store interface (+ cleanup)
- Slim interface: `PutBlock` / `GetBlock` / `GetBlockRange` / `DeleteBlock` / `Walk`.
  Implement on s3 + memory; decorators pass `PutBlock` through and expose the codec transform.
- **Large blocks:** `PutBlock` streams; the S3 store uses **multipart upload** above a
  configurable part-size threshold (mandatory above the 5 GB single-PUT limit) so large
  `TargetBlockSize` values (e.g. 256 MB) work; small blocks use a single PUT. Multipart also
  enables **per-part retry** (re-upload only the failed part) and parallel parts.
- **Cleanup:** remove `Put/Get/GetRange`-by-hash, `FormatCASKey`/`ParseCASKey`, the `cas/`
  walk + keying, and standalone `ReadBlockVerified`.
- **DoD/tests:** conformance suite for the new interface (s3 + memory); range correctness;
  multipart PUT of a large block + ranged GET over a multipart-PUT object.

### P3 — Metadata: block records + locators + batched commit (#1416)
- Per-block record `BlockID → {blockHash, length, liveChunkCount, syncState}`; per-chunk
  locator `hash → {BlockID, Offset, Length}` (rework the existing locator/SyncedHashStore).
- Single-transaction write of all of a block's chunk locators **+** its block record
  (atomic, idempotent) across memory/badger/postgres/sqlite; conformance suite.
- **Subsumes #1416** (batched metadata commits): expose this as the store-contract
  **batched-commit capability** — one fsync per block, not per file — which removes the
  per-file fsync wall #1416 targets. Make the capability the general path, not packing-only.
- **DoD/tests:** atomicity + idempotency; dedup short-circuit; GORM column-coverage care;
  batched-commit capability covered by the store conformance suite.

### P4 — Local tier: log blobs (+ cleanup)
- RocksDB-style log-blob store: append at tail (`pwrite`), read earlier offsets (`pread`),
  per-blob local index (`hash → {logBlobID, rawOffset, rawLength}`), rotation at
  **`MaxBlobSize`** (config, default 1 GB), WAL-style torn-tail recovery (truncate to last
  validated chunk).
- **Hot-cache eviction:** **`MaxLocalStoreSize`** (config) high-water → evict cold,
  **fully-synced** whole blobs (LRU) down to a low-water mark; never evict unsynced. Sub-blob
  reclaim (a blob pinned by a few unsynced/hot chunks) needs **local blob compaction** —
  tracked as #1497 (fast-follow).
- **Cleanup:** remove per-chunk local CAS file storage in `pkg/block/local/fs`.
- **DoD/tests:** crash/torn-tail recovery; threshold eviction reclaims to low-water and keeps
  unsynced resident; concurrent tail-append + carve `pread`.

### P5 — Sync engine: block carving + read path (+ cleanup)
- Real-time carve (block-size or idle) from the log blob, stream transformed+framed bytes
  into `PutBlock`, write locators + block record atomically, mark synced.
- Read path (**local-first → remote on miss → read-ahead**): file offset → chunk → local log
  blob `pread` (hot, zero decode); on miss `GetBlockRange` for the needed bytes (fast first
  byte) → decode → `blake3` verify; then background-prefetch the whole block (and more,
  adaptively) to warm the cache. Coalesce contiguous same-block locators into one ranged GET.
  (See the design's Read path section.)
- **Cleanup:** retire `mirrorChunk` standalone `Put` + standalone `MarkSynced` and the
  standalone-vs-block read branch.
- **DoD/tests:** small files → one block, N locators, correct reads; dedup across files;
  Close carves+uploads the pending tail; crash after `PutBlock` before locator write.

### P6 — GC (delete-only)
- Decrement `liveChunkCount` on the unlink/refcount cascade; at 0 delete the block
  (local + remote) and its record. Remove the old CAS-object GC.
- **DoD/tests:** block deleted only when all its chunks die; no premature deletion.

### P7 — Migration (blocking at startup)
- Detect existing `cas/<hash>` objects (local + remote), re-pack into blocks, rewrite
  locators + block records in one resumable pass, delete old objects. After completion the
  legacy reader (used only here) is the final standalone code path removed.
- **DoD/tests:** `cas/<hash>` fixture → blocks-only, locators rewritten, old objects gone,
  data byte-identical; resumable mid-run.

### P8 — Corruption hardening
- Verify `blake3(block)==blockHash` (from metadata) on fetch; self-heal local from remote
  on mismatch; finalize torn-tail recovery; metrics/alerts on remote mismatch.
- **DoD/tests:** flip a byte locally → recover from remote; flip remotely → fail closed.

## Cross-cutting requirements (apply to every phase)
- Streaming I/O only (`io.Reader/Writer` + `sync.Pool`); never hold a whole block in RAM;
  blocks never written to local disk.
- No compat shims; delete superseded code in the phase that replaces it (definition of done).
- Lint + format + `go test ./...` (+ `-race` on engine) green per PR; sim + review before PR.

## Deferred (already filed)
#1487 GC compaction · #1488 chunk-range refetch · #1489 manifest files · #1490 scrubber ·
#1491 decoupled blob/block sizing · #1492 group-commit fsync.
