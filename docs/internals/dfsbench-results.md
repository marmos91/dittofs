# dfsbench Results — 2026-07-09 (fair run)

Benchmark pass from the `dfsbench` harness (`internal/dfsbench/`, issue #1602), after fixing the
methodology confounds found in the first pass. This supersedes the stale numbers in
[`../BENCHMARKS.md`](../BENCHMARKS.md) for the workloads covered here. Read it as a real but
**partial** data point (dittofs vs one FUSE competitor, one size, single run) — not a full matrix.

## What this run measured

- **Subject:** `dittofs-s3` — DittoFS serving BadgerDB metadata + a local fs cache + an S3 remote
  block store, over its own **native** userspace NFSv3 server (no FUSE, no knfsd).
- **Competitor:** `rclone` — a FUSE writeback mount (`--vfs-cache-mode writes`) re-exported over
  kernel NFS (knfsd): the "FUSE re-export" archetype every S3-FUSE tool shares.
- **Anchor:** `local-disk` — bare fio against a scratch dir (no FS, no S3): the hardware ceiling.
- **Not captured:** `zerofs` (native NFS twin, harness Heisenbug); `juicefs`/`s3fs`/`s3ql`.

Single-tenant Scaleway VM (fr-par-1), Scaleway S3 (Paris), 256 MiB files, 4 fio jobs, 60 s/pass,
NFSv3. Reads run warm then cold (after a per-workload cache evict). Two write modes are run:
`seq-write` (`direct=1`, O_DIRECT/FILE_SYNC) and `seq-write-buffered` (`direct=0`, fsync-at-close).

## Baseline (ceilings)

| | seq | rand-4k |
|---|---|---|
| local-disk (bare scratch) | **1120 MB/s** | **5064 IOPS** |

## Results

| System | Access | Workload | Pass | MB/s | IOPS | p50 ms | p99 ms | CTXSW/s | CPU% |
|---|---|---|---|---|---|---|---|---|---|
| dittofs-s3 | native | seq-write **(direct)** | warm | **6.4** | — | 18.2 | **9865** | 8,237 | 5 |
| dittofs-s3 | native | seq-write **(buffered)** | warm | **242** | — | ~0 | ~0 | 22,880 | 24 |
| dittofs-s3 | native | seq-read | warm | 749 | — | 3.9 | 8 | 34,187 | 40 |
| dittofs-s3 | native | seq-read | **cold** | **159** | — | 1.7 | 575 | 15,467 | 7 |
| dittofs-s3 | native | rand-read-4k | warm | 27 | 7,030 | 13.7 | 76 | 55,429 | 76 |
| dittofs-s3 | native | rand-read-4k | cold* | 23 | 5,879 | 10.4 | 109 | 48,040 | 95 |
| rclone | reexport | seq-write (direct) | warm | 1,756 | — | 2.1 | 3.9 | 156,007 | 56 |
| rclone | reexport | seq-write (buffered) | warm | 996 | — | ~0 | ~0 | 89,864 | 47 |
| rclone | reexport | seq-read | warm | 948 | — | 2.3 | 8.5 | 47,993 | 42 |

*See caveat 3 — the cold rand-read is not genuinely cold. rclone's rand-read/cold cells hit its
documented NFSv3 `ftruncate` flake and are omitted.

## The write story (the whole point of this run)

The first pass reported a "~1900× write gap" and an apparent 1 MB/s DittoFS write wall. **That was
an artifact of the workload, not a DittoFS regression.** Running both write modes settles it:

| seq-write, dittofs native | MB/s | vs its own O_DIRECT |
|---|---|---|
| `direct=1` (O_DIRECT → per-block FILE_SYNC) | **6.4** | 1× |
| `direct=0` buffered (fsync-at-close, UNSTABLE) | **242** | **~38×** |

- **On the realistic buffered path DittoFS writes at 242 MB/s** — healthy, consistent with the
  #1584 UNSTABLE fast-path work and the historical 48–510 MB/s range. So **no, DittoFS is not
  "this bad" at writes.**
- `direct=1` forces a durability barrier the server still fsyncs on — the path #1584 opts out of.
  DittoFS *honours* O_DIRECT (6.4 MB/s, p99 **9.9 s**); rclone's writeback **ignores** it and local-
  acks at 1,756 MB/s. So the direct-mode "gap" is a **durability asymmetry, not speed**.
- But 6.4 MB/s with multi-second per-op latency at 5% CPU is a genuine *stall* in the synchronous
  path — a real bug, tracked separately in **#1621** (candidate causes: synchronous S3 in the
  FILE_SYNC hot path, or the per-store syncLeader stalling when every write demands its own barrier).

Even buffered, rclone (996) leads DittoFS (242) ~4×, but rclone writeback is a dumb local cache
file — no metadata, no FastCDC chunking, no CAS/dedup — whereas DittoFS runs its full durable write
pipeline. Not fully apples-to-apples; 242 MB/s for a real CAS filesystem is the honest read.

## Reads and the FUSE-tax

- **Reads competitive.** Warm seq-read 749 (dittofs) vs 948 (rclone). Genuine **cold-from-S3
  seq-read = 159 MB/s** (the one clean cold number).
- **Native serving is more CPU-efficient.** rclone burns more context switches on *every* workload —
  the tax of crossing kernel↔userspace on a FUSE re-export that a native server avoids:

| workload (warm) | dittofs CTXSW/s | rclone CTXSW/s |
|---|---|---|
| seq-read | 34,187 | 47,993 (+40%) |
| seq-write buffered | 22,880 | 89,864 (+293%) |

## Caveats (read before citing any number)

1. **`direct=1` durability asymmetry.** DittoFS honours O_DIRECT/FILE_SYNC; rclone's writeback
   silently buffers it. Only the **buffered** rows compare like-for-like on durability.
2. **Warm reads are cache-served.** Warm rand-read (7,030 IOPS) exceeds the disk ceiling (5,064) —
   it's the server's RAM read-buffer, not the backend. Only the **cold** reads reflect S3.
3. **Cold rand-read is not truly cold.** `rand-read-4k.fio` lays down its own file (fio writes it
   before reading), so the "cold" pass reads warm just-written data (5,879 ≈ warm 7,030). Only
   `seq-read` cold (which reads the evicted layout file) is a real cold-from-S3 number. Fixing this
   (point rand-read at the shared layout file) is a follow-up.
4. **Single run, no variance.** direct-write measured 1 → 2.6 → 6.4 MB/s across three runs — high
   run-to-run spread. Treat one number as indicative, not precise. 3× repeats are a follow-up.
5. **One competitor, one size.** rclone only (and its rand-read/cold flaked); no zerofs/juicefs;
   256 MiB only; `mixed-rw`/`metadata` not run.

## Harness + product fixes behind this run

- **#1619** (harness): stale `dittofs-s3` recipe rebuilt for a live server; `seq-write-buffered`
  workload; per-workload cold barrier; **teardown-on-setup-failure** so a half-started `dfs` can't
  leak and hold its BadgerDB lock; `prepareMountpoint` resilience.
- **#1620** (product): opening a metadata store whose data dir is already locked now returns an
  actionable error (another dfs is running → stop it / use another path) instead of raw Badger.
- **#1621** (perf): the O_DIRECT/FILE_SYNC write stall.
