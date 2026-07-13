# Cold-read Step 2 — async bounded write-back (design, SOTA-grounded)

**Date:** 2026-07-10
**Status: PARKED — documented lever, NOT scheduled.** Step 1 (#1636) shipped the
best bang-for-buck (+55% cold throughput, p99 566ms→13ms) as a small, low-risk
change. Step 2 helps only cold-from-S3 *sequential* reads over a fat pipe (warm /
local unaffected), for moderate added complexity on a delicate hot path (the
#1362 OOM trap + a read-after-write subtlety). Build this ONLY when cold-from-S3
throughput is a concrete, pulled-for priority (a user complaint or a WAN-streaming
benchmark target) — not on spec. Simpler engine = safer, more maintainable.

**L0 / unified RAM tier: DROPPED.** The OS page cache already is the warm-read RAM
tier (the ~1000 MB/s warm number). An app L0 only adds CAS-dedup RAM caching, for
which we have zero measured demand. Honest follow-up: **delete the dead `cache.go`**
(OnRead never fires, RAM cache never read), don't resurrect it.

**Depends on (if ever built):** Step 1 (#1636) + #1635 (cold-accurate bench).
**Goal (if ever built):** lift cold-from-S3 sequential read from ~115 MB/s toward
the ~688 MB/s 16-wide S3 ceiling by removing the **synchronous `local.Put`** from
the S3-fetch hot path.

## The bug Step 2 fixes

`Syncer.inlineFetchOrWait`: fetch 8 MiB from S3 → **synchronous `local.Put`** →
serve. So cold throughput is capped at `min(S3, disk-write)`, and Step 1's
16-wide prefetch drowns the block volume in 16 concurrent 8 MiB writes. Measured:
1-conn S3 ~168, 16-wide S3 ~688, block volume is the wall. (The `local.Put` was
made synchronous deliberately — a naive goroutine-per-block async Put OOM'd in
#1362, each holding 8 MiB.)

## What the state of the art does (research synthesis)

Every leading S3-backed FS converges on: **serve the reader from the in-RAM
fetch buffer; make the disk-cache write an async, bounded, droppable side-effect.**

- **AWS Mountpoint for S3** — no disk cache by default; prefetches into memory and
  serves from RAM. Disk (`--cache`) is opt-in. Bounded prefetch window (≤2 GiB/handle,
  incremented linearly per read) is the backpressure. Part size **8 MiB**.
  ([CONFIGURATION.md](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md),
  [#488](https://github.com/awslabs/mountpoint-s3/issues/488))
- **Linux fscache/cachefilesd** — the canonical "don't block the read on the cache
  write": serves bytes from the network response while the copy into the on-disk
  cache proceeds independently; **cache-write failures are hidden from the reader**.
  ([kernel docs](https://docs.kernel.org/filesystems/caching/fscache.html))
- **JuiceFS** — async readahead/prefetch off the critical path; `--writeback`
  returns immediately and uploads in the background; 300 MiB shared buffer.
  ([read-perf](https://juicefs.com/en/blog/engineering/optimize-read-performance))
- **GeeseFS/Goofys** — RAM-only aggressive readahead, deliberately no disk cache;
  `--use-enomem` = hard byte ceiling (return ENOMEM rather than exceed the memory
  limit). Read concurrency decoupled from flush concurrency (`--max-flushers`).
  ([README](https://github.com/yandex-cloud/geesefs/blob/master/README.md))
- **Alluxio** — cache **synchronously** only when a block is read fully sequentially
  (worker already holds it); otherwise send an **async cache command** and proceed.
  ([async-caching](https://www.alluxio.io/blog/asynchronous-caching-in-alluxio-high-performance-for-partial-read-caching))
- **rclone VFS** — separate read streams from write-back `--transfers` pool; disk
  write-back is a timer, not inline. ([mount docs](https://rclone.org/commands/rclone_mount/))

**Consensus:** (1) reader served from the fetch buffer, never disk; (2) disk write
decoupled onto a **bounded** background queue; (3) dropping a cache write under
pressure is legal (bytes are S3-durable) — fscache hides cache-write errors.

## Target design (lazy, validated)

The DittoFS fetch path **already serves the reader from the fetch buffer**
(`inlineFetchOrWait` returns `data`, `copyBlockToDest` copies it to dest). So Step 2
is *narrow*: move the `local.Put` off the synchronous path onto a bounded queue.

**We do NOT build a new RAM L0 cache object.** The fetch buffer already is L0 for
the cold pass; the existing `cache.go` RAM cache is dead (OnRead never fires). A
formal RAM read tier earns its place only for *re-read locality* — defer until
measured. (Research rec E-5.)

### Components
1. **Bytes-budget semaphore** — a single ceiling on in-flight write-back bytes
   (default ~256–512 MiB ⇒ 32–64 blocks of 8 MiB). Sized in **bytes not count**
   (geesefs `--use-enomem` pattern). Caps RAM deterministically.
2. **Small fixed writer pool** — 2–4 goroutines draining a bounded channel of
   `{hash, data}`, doing `local.Put` + `markFetchedSynced`. **Narrow on purpose**:
   decouples 16-wide S3 fetch from a few disk writers (geesefs/rclone pattern).
3. **Drop-on-pressure** — if the budget is exhausted / queue full, **skip the Put**
   (non-blocking). The block stays S3-resident; a later demand re-fetches it. Safe
   and precedented (fscache). Never throttle the reader to disk speed.

### The one real correctness subtlety
`EnsureAvailableAndRead` today: if a read spans a local block AND a freshly-fetched
block, it sets `needLocalReadAt` and returns `filled=false`, so the caller
**re-reads the whole range from local** (`readLocalByHash`). With an async Put, the
just-fetched block may **not be on disk yet** → the re-read misses it (zero-fill /
re-fetch). So Step 2 must **always direct-serve a fetched block** and never route it
through a full-range local re-read. Options:
- **(a)** Restructure so fetched blocks are always `copyBlockToDest`'d and only
  genuinely-local blocks use the local read path (no all-or-nothing `needLocalReadAt`).
- **(b)** Keep a small **pending-write-back RAM map** (hash→bytes) that `readLocalByHash`
  consults before falling through — covers concurrent/random re-reads of an in-flight
  block too. Bounded by the same bytes budget.
Start with **(a)** (pure sequential single-pass needs nothing more); add (b) only if
concurrent-reader re-reads show up as re-fetch amplification.

### Config
- Reuse Step 1's knobs; the writer pool width + byte budget derive from existing
  config (e.g. `ParallelDownloads` for fetch, a new small constant for writers).
- **(Defer, E-4)** optional per-share "no local cache on read" bypass for known-
  streaming shares — serve straight from S3, skip the write-back entirely. Cheap
  insurance; only if a workload wants it.

## Plan (VM-measured, same method as Step 1)
1. **Build** the bytes-budget semaphore + writer pool; route `inlineFetchOrWait`'s
   Put through it; fix the direct-serve/re-read interaction (option a).
2. **Deterministic tests** — budget never exceeded; drop-on-full; read-after-write
   within the async window resolves; `-race`.
3. **VM A/B** (dfsbench, cold-accurate per #1635): expect cold 115 → toward S3
   ceiling; watch `DiskWrMB/s` (should stop being the wall) and `NetRxMB/s`
   (should rise toward read rate). If flat, instrument the write-back queue
   (depth, drops, in-flight bytes).

## Guardrails
- **#1362 OOM** — the whole point of the byte budget; never unbounded goroutines.
- **Durability unchanged** — write-back bytes came from S3 (already durable); a
  dropped Put loses only a cache-population, never data. Do NOT touch the WRITE
  path (client writes still sync to local per the WRITE ordering invariant).
- **Per-share isolation** — the queue lives on the per-share `*Syncer`.
- **Pair every throughput claim with a real-VM A/B** (the lesson of this whole saga).
