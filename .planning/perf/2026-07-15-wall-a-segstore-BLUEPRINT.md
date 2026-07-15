# Wall A — `segstore` local block store redesign: architecture blueprint

> Finalized executor-ready blueprint for Wall A of the three-wall NFS/SMB perf plan
> (`2026-07-15-nfs-smb-three-walls-PLAN.md`). All design decisions are LOCKED in the plan;
> this fills in every format / interface / algorithm / PR detail. Self-contained — executors
> can implement from this without prior conversation context.

Locked spec: `2026-07-15-nfs-smb-three-walls-PLAN.md` (Wall A §1–§6 + Unifying model + rationale).

## 1. Package layout

New standalone package: **`pkg/block/segstore/`** — owns all persistent local-cache state, zero imports from `pkg/controlplane`, `pkg/metadata`, or protocol packages. Only external deps: `pkg/block/remote` (for the `RemoteBlockStore` shape it borrows, not imports directly — see §2) and stdlib.

```
pkg/block/segstore/
  segstore.go   // Store struct, Open/Close/Stats, Config, Clock
  record.go     // record framing, encode/decode, CRC
  segment.go    // active/sealed segment lifecycle, sealed-fd pool, append primitive
  index.go      // per-file interval index, SegmentLocation, DataExtents
  shard.go      // fileID -> shard partitioning (FNV-1a, 2^N)
  carve.go      // FastCDC+BLAKE3+dedup-at-carve, batching trigger
  evict.go      // pressure-gated whole-segment eviction
  gc.go         // dead-byte tracking, size-tiered repack
  recovery.go   // tail-scan, CRC validate, torn-write truncate, orphan sweep
  doc.go
```

Replaces, in `pkg/block/local/fs/`: `rollup.go`, `compaction.go`, `appendlog.go`, `appendwrite.go`, `logindex.go`, `readpayload.go`, `eviction.go`, `recovery.go` — all deleted (§6). Replaces `pkg/block/local/logblob/` entirely (deleted). `pkg/block/local/fs/sync_leader.go` and the shard-hashing idea in `logshard.go` are **ported, not deleted** — they're already format-agnostic (§6).

`FSStore` (surviving, slimmed) becomes a thin adapter: it still implements `block.Store` / `block.BlockStoreAppend` / `DurabilityReporter` (`pkg/block/blockstore.go`) for the rest of the codebase, but every method body delegates to a `*segstore.Store` instance plus the existing `metadata.LocalChunkIndex` bookkeeping on the other side of the seam.

## 2. Public API

```go
package segstore

type FileID string // == today's payloadID; same value space, same shardFor hash

type Store struct { /* unexported */ }

func Open(dir string, cfg Config, remote RemoteStore, clock Clock) (*Store, error)
func (s *Store) Close() error

// Client-driven dirty write. Never fsyncs; returns once buffered.
func (s *Store) WriteAt(ctx context.Context, id FileID, offset int64, data []byte) error

// Cold-read hydration write. Same append primitive as WriteAt, synced=true.
func (s *Store) Hydrate(ctx context.Context, id FileID, offset int64, data []byte) error

func (s *Store) ReadAt(ctx context.Context, id FileID, offset int64, dst []byte) (n int, cold bool, err error)

// Fsync-durable checkpoint. NFS COMMIT / SMB Flush land here.
func (s *Store) Commit(ctx context.Context, id FileID) error

// Explicit/forced carve trigger; background loop also calls this per-shard.
func (s *Store) Carve(ctx context.Context, opts CarveOptions) (CarveResult, error)

// Pressure-gated whole-segment eviction; targetBytes=0 means "one segment if any qualifies".
func (s *Store) Evict(ctx context.Context, targetBytes int64) (EvictResult, error)

func (s *Store) DataExtents(ctx context.Context, id FileID, fileSize int64) ([][2]uint64, error)
func (s *Store) Delete(ctx context.Context, id FileID) error
func (s *Store) UnsyncedBytes() int64
func (s *Store) Stats() Stats
```

Injected dependencies:

```go
// Mirrors pkg/block/remote/remote.go:74 RemoteBlockStore in shape; segstore
// depends on this narrow interface, not the remote package's concrete type.
type RemoteStore interface {
    PutBlock(ctx context.Context, id BlockID, r io.Reader, size int64) error
    GetBlock(ctx context.Context, id BlockID) (io.ReadCloser, error)
    GetRange(ctx context.Context, id BlockID, off, length int64) (io.ReadCloser, error)
}

type Clock interface { Now() time.Time }

type Config struct {
    SegmentSize      int64         // fixed segment cap before rotation (e.g. 256MiB)
    CarveBlockSize   int64         // fixed pack size, bench param, START 4 MiB
    CarveMaxAge      time.Duration // age-based batching cap
    GCDeadRatioForce float64       // e.g. 0.5, forces repack regardless of scheduling
    ShardCount       int           // 2^N, immutable per store instance (§8)
}
```

`WriteAt` and `Hydrate` are two thin public entry points over one internal `appendRecord(id, offset, data, synced bool)` primitive — the plan's unifying model made concrete: client writes and S3-hydration both funnel through the same append path, differing only in the `synced` bit.

## 3. On-disk formats

**Segment header** (offset 0 of every `.seg` file):

```
Magic       [8]byte  "DFSSEG1"
SegmentID   uint64   // matches filename
CreatedAt   int64    // unix nanos
Flags       uint32   // bit0 = sealed (immutable; header is truth on recovery)
HeaderCRC32 uint32
Reserved    [32]byte // room for future fields without reformatting
```

Filename: 16-digit zero-padded decimal `SegmentID` + `.seg` — reuses the exact blob-ID naming convention already used by `pkg/block/local/logblob/manager.go`.

**Record** (append-only stream following the header):

```
MagicByte    uint8   // 0xD5, torn-write scan anchor
HeaderLen    uint8   // versioned, currently 29
FileIDLen    uint16
FileOffset   uint64  // logical offset within the file
PayloadLen   uint32
Version      uint64  // global monotonic LSN, assigned at append; NEVER reissued on repack
Flags        uint8   // bit0=synced (clean/evictable), bit1=tombstone
HeaderCRC32  uint32  // covers Magic..Version ONLY — deliberately excludes Flags
FileID       []byte  // FileIDLen bytes
Payload      []byte  // PayloadLen bytes
PayloadCRC32 uint32  // covers Payload only; verified on GC/repack/recovery, NOT warm read
```

`HeaderCRC32` excluding `Flags` is load-bearing: carve-completion flips the synced bit in-place (single `pwrite` at the record's known `SegOffset`) without invalidating the CRC or rewriting the record. `Version` is a store-wide atomic LSN, not a per-segment sequence — this is what makes newest-wins well-defined after GC/repack physically relocates records into a different segment.

Departure from today's per-payload single-log-file model (`fs/logshard.go:19`): today one log file == one payload, so no `FileID` in the frame. The new format packs **many files into one shared segment**, so `FileID`/`Version`/explicit `synced` flag move from implicit to record-resident.

**`.idx` sidecar** (`<segmentID>.idx`, flat array of 40-byte fixed entries, lazy/best-effort — `COMMIT` fsyncs only the `.seg`, never the `.idx`):

```
FileIDHash  uint64  // FNV-1a of FileID, same hash fs/logshard.go:46-55 already uses
FileOffset  uint64
PayloadLen  uint32
Version     uint64
SegOffset   uint64  // byte offset of the record in the .seg
Flags       uint8
pad         [7]byte // align to 40 bytes
```

Losing `.idx` is a performance event only: fully rebuildable by re-scanning the sibling `.seg` (§5 Recovery). Record overhead ~69 bytes fixed + variable `FileID` (~36 bytes) — a tiny-scattered-write cost, mitigated (not eliminated) by A5 record-merge.

## 4. In-memory structures

**Per-file interval index** — reuse the existing `intervalTree`/coverage-set shape (`fs/logindex.go`, `fs/logshard.go:22-23`), rekeyed: each node stores `SegmentLocation{SegmentID uint64, Offset int64, Length int64}` instead of `LocalChunkLocation` (`pkg/block/block_record.go`), because entries can no longer assume "this is my own payload's file" — segments are shared. Newest-wins is enforced by node overwrite on WriteAt/Hydrate.

**Per-segment metadata**, one struct per known segment, in `segmentTable map[uint64]*segmentMeta`:

```go
type segmentMeta struct {
    id             uint64
    sealed         atomic.Bool
    liveBytes      atomic.Int64
    deadBytes      atomic.Int64 // Titan discardable_size analog
    syncedRecords  atomic.Int64 // eviction synced-gate check (§5)
    lastAccess     atomic.Int64 // unix nanos, approx-LRU eviction victim key
    quotientFilter *qf.Filter   // membership hint, rebuilt on every repack (§8)
    fd             *os.File     // sealed-fd pool entry, same pattern as logblob.Manager
}
```

**Active-vs-sealed model**: exactly one active segment per shard accepts appends. On `SegmentSize` overflow or `CarveMaxAge` the segment is sealed — fsync **before** flipping the header's sealed bit (mirrors `logblob/manager.go` Rotate's durability-boundary) — and a new active segment opens. Sealed segments read-only until GC repacks.

**Partition sharding**: `ShardCount` (2^N, default 16 matching `fs/logshard.go:9`) shards keyed by the identical FNV-1a-masked hash of `FSStore.shardFor` (`fs/logshard.go:45-55`). **Each shard owns its own active segment** — N shards write to N concurrently-appendable segments, no single global active-segment lock. GC and carve iterate shards independently.

## 5. Algorithms

**Write + COMMIT**
1. `shard := shardFor(fileID)`; acquire shard's single active-segment append mutex.
2. Frame the record: `Version = atomic global LSN++`, `Flags.synced = false`.
3. `pwrite` into the shard's active segment at the tracked tail offset (single writer per shard).
4. Best-effort append the 40-byte `.idx` entry (failure logged Warn, never fails the write).
5. Update the per-file interval tree: insert/overwrite covering node → new `SegmentLocation`; bump superseded node's segment `deadBytes`.
6. Return without fsync (dirty/buffered).
7. On `Commit(fileID)`: route through the existing store-level `syncLeader` (`fs/sync_leader.go:9-22`, ported verbatim — coalesces `fsync()` closures, format-agnostic) for the shard's active fd. Durability is implicit in "fsync happened".

**Carve** (per shard, background worker + explicit `Carve()`)
1. Snapshot covering dirty intervals for files whose dirty-byte count crosses `CarveBlockSize` or oldest dirty record exceeds `CarveMaxAge`, under the shard lock — same Phase-A/B/C split as `rollup.go`'s `rollupFileInner`.
2. Lock-free: stream dirty bytes in file-offset order through FastCDC → BLAKE3 → per-share dedup (`block.EngineFileChunkStore`, `pkg/block/filechunk.go:145`, unchanged) → novel chunks accumulate.
3. At `CarveBlockSize`/`CarveMaxAge`: seal each novel chunk (`chunkSealer`, `engine/syncer.go:139-142`), frame via `blockcodec.Builder` (#1414, reused), `PutBlock`.
4. On success: atomic commit (block record + locators + synced markers — `blockCommitter`, `engine/syncer.go:179-189`). **Then** flip each carved record's `Flags.synced` in-place (single-byte `pwrite`, CRC untouched). Flip-after-commit means a crash between commit and flip causes one harmless re-carve, never data loss.
5. Per-shard `carveMu` serializes a shard's flush against a concurrent explicit `Carve()`.

**Warm read**
1. Look up covering intervals for `[offset, offset+len(dst))`.
2. For each hit, `pread` from `segmentMeta.fd` or active fd — brief-lock-then-unlocked-pread (same as `logblob.Manager.ReadAt`).
3. Uncovered: true POSIX hole → zero-fill; known-written-but-evicted → cold read.
4. **No hash-verify** — record CRC checked only by GC/repack/recovery. Retires #1648 verify-once (moot: no separate CAS-verify step).

**Cold read (serve-direct + async hydrate, A8)**
1. On an interval-index miss for a range known remote (resolved by the FSStore/engine caller via logical-offset→hash→`ChunkLocator`; segstore has no namespace dependency), the caller does `GetRange`/`GetBlock` and serves the client immediately — never blocks on a local write.
2. Fetched bytes copied to wire first; only then `segstore.Hydrate` (buffer-ownership discipline).
3. Hydrate **every** chunk unpacked from the fetched block — free prefetch.
4. Concurrent cold readers singleflight at the caller's read-through-cache (#1362).

**Eviction** (lazy, pressure-gated only)
1. Trigger: `LocalStoreSize` threshold, `dfsctl` force, or near-full fallback.
2. Victim: coldest sealed segment by `lastAccess`, filtered to `syncedRecords == liveRecords`. Reuses `logblob.EvictBlob`'s synced-gate.
3. On eviction: unlink `.seg`+`.idx`, drop `segmentMeta`, and for every interval-tree entry pointing into it, **replace (not delete)** with a "cold, fetch remote" marker — so a read falls to cold-read, not a false hole.
4. If pressure persists with no evictable segment (dirty pinned): surface via `UnsyncedBytes()`, backpressure the write path, log loud+actionable Warn.

**GC/repack**
1. `deadBytes` increments when an interval-tree node is overwritten or a file tombstoned.
2. Size-tiered victim: highest `deadBytes/SegmentSize`; `GCDeadRatioForce` forces immediate repack (RocksDB #9235 space-amp lesson).
3. Repack (from `compaction.go`'s `compactLogLocked`): snapshot live records → write into repack-target segment **preserving `Version` and `synced`** (never reissued) → fsync dest → fsync dir → update interval-tree entries → **only then** unlink source.
4. Ordering: bytes-durable-before-index-durable-before-reclaim (identical to `eviction.go`'s `relocateSurvivors`). Crash before unlink → harmless orphan, swept later.

**Recovery**
1. Per shard: locate the one unsealed (active) segment (header Flags bit0 unset).
2. Tail-scan from offset 0 (bounded by `SegmentSize`): validate `HeaderCRC32` then `PayloadCRC32`; truncate + fsync at first invalid/truncated record.
3. Replay valid records into a fresh interval tree and fresh `.idx`.
4. Recompute global Version LSN as `max(observed Version)+1`. Sealed segments trusted via header Flags ("header is truth") — no full re-validation each boot; only the active segment gets the tail-scan.
5. Orphan sweep: age-gated; any on-disk segment unreferenced by bookkeeping is redundant (per GC ordering invariant), safe to delete after the age gate.

**Migration (A6)**
Drain→discard→re-hydrate, no format converter. Upgrade blocks until `UnsyncedBytes()==0`, then starts segstore with an **empty** local cache — first post-upgrade reads are cold and repopulate via Hydrate. One-shot, deleted the release after it ships (precedent: `fs/legacy_migration.go`).

## 6. Mapping to existing code

**Delete:** `pkg/block/local/fs/{rollup,compaction,appendlog,appendwrite,logindex,readpayload,eviction,recovery}.go`; `pkg/block/local/logblob/` (entire package — reusable patterns reimplemented against the shared-segment format in `segstore/segment.go`).

**Modify:**
- `pkg/block/local/fs/chunkstore.go`, `blockstore_methods.go` — read/write bodies (`chunkstore.go:185,251,300`) delegate to `segstore.Store.ReadAt`/`WriteAt`/`Hydrate`.
- `pkg/block/block_record.go` — rename `LocalChunkLocation{LogBlobID,RawOffset,RawLength}` → `SegmentLocation{SegmentID uint64, Offset, Length int64}`; mechanical across callers (`carver.go:293-299`, `eviction.go:381/498/555/744`, `legacy_migration.go:120/270`, `blockstore_methods.go:71/126`).
- `pkg/block/engine/carver.go`/`syncer.go` — **biggest boundary shift.** Chunk-boundary discovery moves to carve time, inside `segstore.Carve()`. **Delete** `localBlobReader` (`syncer.go:191-198`) + field (`syncer.go:151`); segstore does its own local reads. `engine/syncer.go` keeps only packing+PutBlock+commit: calls `segstore.Carve()`, gets chunked/hashed novel batches, frames via `blockcodec.Builder` + `PutBlock` + `blockCommitter`. Delete `pendingCarveHashes`/`carveQ`/`addPendingHash` (`syncer.go:100-230`) and the `onChunkComplete` ingest hook — no chunk-at-ingest anymore.
- `pkg/controlplane/runtime/shares/service.go` — `CreateLocalStoreFromConfig` constructs segstore-backed `FSStore`; delete dead `use_append_log` flag.
- `pkg/block/blockstoretest/conformance.go`, `appendlog.go` — kept; four currently-skipped scenarios (`PressureChannel_INV05`, `TornWriteRecovery_LSL06`, `ConcurrentStorm`, `RollupOffsetMonotone_INV03`) get their fs-internal probes rewritten against segstore internals (PR3/PR8), not dropped.

**Keep unchanged:** `metadata.Transactor`/`SyncedHashStore`/`LocalChunkIndex`; `remote.RemoteBlockStore` (`remote/remote.go:74`); `block.EngineFileChunkStore` (`filechunk.go:145`); `blockcodec.Builder`; `fs/sync_leader.go` (ported as-is). `engine/{gc,gc_block,reclaim,dataextents,readahead,cache,sync_health}.go` — flagged for a targeted read-through to confirm none hide `LocalChunkLocation`-shaped assumptions (open item).

## 7. PR sequence

Each independently reviewable, conformance-gated where applicable, `-race` clean, simplifier→reviewer→lint→PR→CI-green→squash-merge.

0. **PR0 — standalone bench harness.** `pkg/block/segstore/` skeleton + in-package benchmarks vs a fake in-memory `RemoteStore`, no wiring. Acceptance: `go test -bench=.` produces write/read/carve throughput; the baseline every later PR is measured against.
1. **PR1 — segment format + append primitive.** `record.go`, `segment.go`: header, framing, active/sealed, sealed-fd pool, `.idx` writer, `Open`/`WriteAt`/`ReadAt`/`Commit`/`Close`/`Stats`. Acceptance: random offset/length spans across many `FileID`s byte-identical readback; crash-injection mid-append → clean recovery truncation; `-race`.
2. **PR2 — interval index.** `index.go`: tree rekeyed to `SegmentLocation`, hole/gap, `DataExtents`. Acceptance: sparse/hole/newest-wins-after-out-of-order conformance.
3. **PR3 — recovery.** `recovery.go`: tail-scan, CRC, torn-write truncate, Version-LSN recompute, orphan sweep. Acceptance: `TornWriteRecovery_LSL06` becomes a real passing test.
4. **PR4 — carve.** `carve.go`: FastCDC+BLAKE3+dedup-at-carve, size+age batching, in-place synced-bit flip. Acceptance: chunk boundaries match a reference FastCDC/BLAKE3 run; re-carve-after-crash no double-count; dedup vs fake `EngineFileChunkStore`.
5. **PR5 — eviction.** `evict.go`: pressure-gated whole-segment, synced-gate, cold-marker invalidation. Acceptance: evicting a synced segment leaves reads on its ranges reporting cold (not hole/error); `DataExtents` still reports range present.
6. **PR6 — GC/repack.** `gc.go`: dead-byte hooks, size-tiered pick, `GCDeadRatioForce`, fsync-then-unlink. Acceptance: forced repack fires above threshold without idle wait; crash before unlink → harmless orphan, data intact.
7. **PR7 — engine wiring.** Rework `engine/syncer.go`/`carver.go` (segstore owns chunking); delete `localBlobReader`, `pendingCarveHashes`/`carveQ`, `onChunkComplete`. Acceptance: `carver_test.go` passes new call shape; e2e `BlockStoreConformance` green through the real `Syncer`. **Riskiest interface change — extra reviewer scrutiny.**
8. **PR8 — FSStore adapter rewrite.** Delegate to segstore; `LocalChunkLocation`→`SegmentLocation` rename; delete the eight `fs/*.go` files + `logblob/`. Acceptance: full conformance green, `-race`, compile-gate on no dangling refs.
9. **PR9 — share wiring.** `CreateLocalStoreFromConfig` segstore-backed by default (no flag); delete dead `use_append_log`. Acceptance: NFS+SMB e2e green.
10. **PR10 — migration.** One-shot drain-then-cold-start (A6), deletion-scheduled next release. Acceptance: e2e upgrade test — drain to `UnsyncedBytes()==0`, post-upgrade cold reads correct.
11. **PR11 (deferred, A7) — O_DIRECT + io_uring** for background carve/GC bulk reads, Linux-only, gated behind post-PR10 profiling.

## 8. Open risks / edge cases

- **Crash mid-append with CRC-coincidence.** A torn record whose `HeaderCRC32` validates while `PayloadLen` points past a plausible stale EOF — guard with a `PayloadLen` sanity ceiling (e.g. `CarveBlockSize`-scale), not CRC alone.
- **Crash mid-carve, between commit and synced-flip.** Record still reads `synced=false` on restart → harmless re-carve; content-addressed dedup makes the re-commit a no-op. Flip strictly after commit → always errs toward "re-carve", never data loss.
- **Concurrent overwrite during carve.** A newer-`Version` write mid-scan lands elsewhere; the carve snapshot (under shard lock, released before CDC) packs the range it named by `SegOffset`, flip only touches those records; the overwrite carves next pass. One extra round, no bug — matches `rollup.go` today.
- **Dirty-can't-evict backpressure.** Threshold/backoff curve left a `Config` tunable; start by porting `eviction.go`'s `ensureSpace` hard-gate rather than inventing a curve.
- **Quotient-filter false positives.** Safe by construction (no false negatives); a FP costs one wasted pread. The filter **must be rebuilt, not carried, on every repack** — explicit in `gc.go`.
- **Partition-shard boundaries immutable.** `ShardCount` fixed at `Open()`; changing it misroutes lookups. Online resharding out of scope — document create-time-only; a dedicated rehash-relocate migration if ever needed.
- **Shared-segment blast radius.** Segments multiplex many files, so a corrupted/lost segment now affects many files, not one. Accepted trade (for S3-packing + coarse eviction) — raises the bar on recovery per-record CRC rigor.
- **Record header overhead (~69 B/record).** Meaningful for many-tiny-scattered-writes; A5 record-merge mitigates but is rate-gated — a tiny-writes-then-COMMIT burst before merge pays full overhead. Explicit PR0 benchmark case.
- **Correlated `.idx` loss.** All shards' `.idx` lost at once → recovery re-scans every sealed segment, not just active. Emit loud Warn ("N segments missing .idx, rebuilding, may take a while").
- **`BlockReclaimer` concurrent-GC hazard (from prior memory).** The union refcount-decrement is unsafe under concurrent GC (single sweeper lock is per-`GCStateRoot`, not per-remote) — must be made safe when the segment store becomes the reclamation target.
- **`engine/syncer.go` rework (PR7) is the single riskiest step** — changes who calls whom across the carve seam. Own acceptance tests beyond the conformance gate ("minimize interface surface" house rule).
