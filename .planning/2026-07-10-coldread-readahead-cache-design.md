# Cold-read readahead + two-tier cache — design

**Date:** 2026-07-10
**Branch:** `perf/coldread-onread-prefetch-depth` (off develop, includes #1630 drain-fix)
**Goal:** raise NFS/SMB cold-from-S3 sequential read throughput from the current
~167 MB/s (single-S3-connection) toward the measured aggregate ceiling (~688 MB/s
at 16-wide S3 concurrency), by keeping the read-serving tier populated *ahead* of
the reader.

---

## Measured facts (don't re-litigate)

- **Cold NFS seq-read ≈ 167 MB/s**; warm ≈ 1058 MB/s. Cold CPU 9–11% → **network-bound, not CPU-bound**.
- **Raw in-region Scaleway S3 probe** (8 MiB GETs, DEV1-S, fr-par):
  - conc=1: p50 50ms, p99 **688ms**, ~168 MB/s
  - conc=4: ~500 MB/s aggregate
  - conc=16: p50 194ms, p99 514ms, **~688 MB/s aggregate**
  - conc=32: p50 420ms → **per-fetch latency inflates past ~16; aggregate plateaus ~640** (over-subscription hurts).
- So: **167 == single-connection S3 bandwidth**, and the 566ms cold p99 tail == **inherent S3 GET tail latency** (raw p99 688ms). The engine can only *hide* the tail, not remove it.
- **~4× headroom** exists (167 → ~688) *iff* prefetch sustains ~16 in-flight S3 fetches without over-subscribing.

## Root-cause findings (why 2 prior attempts were flat/worse)

1. **The RAM `Cache` (cache.go) is never consulted on reads.** `readAtInternal` serves from the local store; comment: *"the cache is hint-only and does not serve bytes here."* On the VM that is a **3.9 GiB (`ReadBufferSize = mem/8`) RAM cache allocated and prefetched-into but never read** — dead weight.
2. **The cache's prefetch loader is local-only:** `loadByHash = bs.local.Get(...)`. It never fetches from S3, so for a cold block it is a **no-op**. Its 4 `PrefetchWorkers` copy already-local blocks into the unread RAM cache.
3. **`OnRead` prefetches the wrong hashes** — the last `depth` *already-read* tail hashes (`allHashes[len-want:]`), not future blocks, and with a geometric ramp (1→2→4→8, cap `maxPrefetchDepth=8`).
4. **The read-serving prefetch (SyncQueue `enqueuePrefetch`) fires only on a local *miss*** (inside `EnsureAvailableAndRead`), **after** the inline demand fetch completes, with a geometric ramp. So the reader consumes an 8 MiB block in ~30ms of local sub-reads but each S3 fetch is ~50–566ms → the reader **outruns** the just-enqueued prefetch and demand-fetches every block **serially → conc-1 → 167**.
5. **#1628 refuted (167→122):** it added per-read `blockIsLocal` probes on the hot path (p50 3.8→29ms) and risked demand+prefetch **over-subscription** (probe: conc=32 → per-fetch 420ms). Directionally right (slide window every read + piggyback demand on prefetch) but in the wrong place (`EnsureAvailableAndRead`, only on miss) and over-subscribing.
6. **#1572 (O(N²) manifest enumerate + Sscanf) is largely stale on badger** — fixed by #1587 (indexed `GetFileChunkAtOffset`) + the hand-rolled `ParseChunkOffset`. Cold pass CPU 9–11% confirms it is not the cold cause. May still bite fragmented files on memory/sqlite/postgres (ListFileChunks fallback) — separate/minor.

## Effective config (auto-deduced, POP2-8C-32G)

| deduced | value | maps to | notes |
|---|---|---|---|
| `ReadBufferSize` mem/8 | 3.9 GiB | cache.go `Cache.maxBytes` (L0) | **dead today** — becomes real L0 |
| `ParallelFetches` cpus×2 | 16 | `ParallelDownloads` = SyncQueue workers + fetchGroup limit | the S3 concurrency knob; probe sweet spot |
| `PrefetchWorkers` fixed | 4 | cache.go `NewCache` workers | useless; retire/repurpose |
| `PrefetchBlocks` | (engine default 64) | readahead window depth | window size lever |

## Best-practice / competitor patterns adopted

- **JuiceFS**: RAM buffer (L0) → disk cache (L1) → object store; sequential readahead of N blocks concurrently into the buffer, flushed to disk.
- **rclone VFS full**: per-file readahead; **growing read-chunk (small first → double)** for fast TTFB then throughput; disk cache.
- **Alluxio**: MEM→SSD→HDD→UFS tiers, async caching, CACHE_PROMOTE.
- **AWS S3**: **parallel byte-range GETs**, ~1 conn ≈ 90 MB/s, scale horizontally (but respect the client/link aggregate ceiling — over-subscription inflates latency).
- **Linux kernel**: async readahead **window that ramps on sequential detection and stays ahead** of the app; non-blocking.

Consensus = exactly the user's instinct: **two-tier cache (RAM L0 + disk L1) + one async sequential readahead that keeps the durable tier populated ahead of the reader with bounded high concurrency.**

## Target design

- **L0 = RAM `Cache`** (wire cache.go into `ReadAt` — first tier; reuse the 3.9 GiB).
- **L1 = local block store** (disk — durable, the read-serving tier).
- **L2 = S3.**
- **Read path**: `ReadAt` → L0 `Cache.Get` → L1 `local.ReadPayloadAt` → L2 fetch (populate L1 **and** L0).
- **One prefetch driver** (retire the two half-working ones): on **every** read, sequential detection maintains a **fixed window of `W` blocks fetched S3→L1 ahead of the read frontier**, using a **dedicated bounded-concurrency (≈16) fetcher** that is **shared with / piggybacked by demand** so total S3 concurrency stays ≈16 (no over-subscription). Hot blocks promoted to L0.

**Dead-code cleanup is part of this work, not a follow-up.** The cache.go `OnRead`
prefetch trigger never fires (hot path passes `blocks=nil`), the RAM `Cache` is
never read by `readAtInternal`, and `loadByHash` is local-only. Whatever we don't
wire in Step 2, we **delete** — no unwired subsystem left in place (it wasted a VM
investigation already). Same for the on-miss SyncQueue `enqueuePrefetch` read-
prefetch once the offset-based driver replaces it. One live driver, zero dead
prefetchers. (Rule: clean never-called paths as found.)
- **TTFB**: demand-fetch the covering chunk first (unchanged, ~one chunk RTT); readahead runs behind it so the *stream* stays full.
- **Predictability**: fixed window `W`, fixed concurrency, explicit sequential trigger, aggressive (non-geometric) ramp once sequential is confirmed. Random access → window 0 (no wasted GETs).

Key invariants to preserve: per-share isolation (CLAUDE.md rule 4); block stores per-share; opaque handles; error codes; `-race` clean; local writes DISJOINT dest regions; in-flight dedup so demand never double-fetches a prefetched block.

## Iterative plan (VM-measured each step)

Measurement is trustworthy now (#1630 drain-fix lands cold passes; #1627 drain-before-evict). Method: same-VM old-vs-new, POP2-8C-32G fr-par, fresh bucket per run, `dfsbench run --remote --systems dittofs-s3-nfs3 --workloads seq-read --sizes large`, compare cold MB/s + p50/p99. Re-push NEW binaries via scp-to-temp + `mv` (avoid ETXTBSY). Tear down with `with-block=true` (not `with-volumes`).

1. **Step 1 — cold lever (build first):** single async S3→L1 sliding-window readahead, fires every read, fixed deep window, 16-wide, demand piggybacks (no over-subscription). Retire SyncQueue `enqueuePrefetch` read-prefetch + cache.go local-only prefetch. **Target: 167 → toward ~688.** If flat → instrument (prefetch hit/miss, in-flight gauge, per-fetch latency) before more changes.
2. **Step 2 — wire L0:** `Cache.Get` into `ReadAt`; populate L0 on read + on prefetch; make `loadByHash` (or the prefetch path) fetch S3 when cold. Measure warm/repeat + TTFB.
3. **Step 3 — tune** window `W` and concurrency to the measured S3 tail; confirm no over-subscription regression; verify random-IO unaffected (planReadahead → 0).

## Open questions / risks

- **Over-subscription**: demand + prefetch must not exceed ~16 concurrent S3 (probe: 32 inflates per-fetch to 420ms). Enforce a *shared* concurrency budget.
- **Startup convergence**: reader must not stay demand-bound during window ramp; start aggressively once sequential confirmed.
- **`ReadAt` `blocks` arg — RESOLVED:** the hot NFS/SMB path (`internal/adapter/common/read_payload.go:62`) passes **`blocks = nil`**, so `Store.ReadAt`'s `len(blocks) > 0` guard skips `OnRead` entirely → **cache.go `OnRead` never fires on real reads** (100% dead, trigger + serving both). ⇒ the readahead driver MUST be **offset/frontier-based in the read path**, resolving forward blocks via indexed `GetFileChunkAtOffset` (not caller ChunkRefs). Fire it in `Store.ReadAt`/`readAtInternal` (runs every read, incl. local hits) — not gated on the nil `blocks` arg.
- **RAM pressure**: L0 = 3.9 GiB is large; ensure L0 population doesn't evict useful L1 or OOM (bounded, LRU already there).
- **Third refutation guard**: pair every throughput claim with a real-VM A/B; a structural test proving the window slides is necessary but NOT sufficient (lesson from #1625/#1628).

## References

- Memory: `project_coldread_serial_demand_1625.md` (full refutation history + probe).
- Shipped: #1630 (drain-uploads write-deadline — unblocks cold measurement), #1627.
- Closed not-planned: #1628 (prefetch pipelining), #1625 thesis.
