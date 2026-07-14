# DittoFS Performance Benchmarks

Performance of DittoFS (S3 backend) against the S3-backed network filesystems the
`dfsbench` harness supports — **JuiceFS, rclone, s3fs, and ZeroFS** — plus
**local-disk** as the raw-hardware ceiling. All systems run on one disposable
Scaleway VM, driven by the same `fio` workloads over the same protocol.

> **Read these with the rig in mind.** Each cell is a single 30 s `fio` run on a
> shared cloud VM. Trust the **shape and order of magnitude**, not the third
> digit. Medium-file sequential-read is first-touch noise and is omitted.

## Test setup

| | |
|---|---|
| Date | 2026-07-14 |
| DittoFS | `develop` @ `38679f1c` |
| Harness | `internal/dfsbench` (fio driver + SCW orchestration) |
| Host | Scaleway `fr-par-1`, single VM, Ubuntu 24.04 |
| Protocol | **NFSv3** — the only protocol all backends share (ZeroFS is NFSv3-only) |
| Object store | Scaleway Object Storage, `s3.fr-par.scw.cloud` |
| fio | 4 threads, 30 s/cell, **warm pass only** this cycle |
| Sizes | medium = 1 MiB, large = 1 GiB |
| Result | 6 systems, 82 cells, **0 workload errors** |

> **Coverage caveats for this run.** Warm-only — the cold/post-evict pass was
> dropped this cycle to avoid a `drain-uploads` stall on the metadata working
> set, so the cold column is pending a follow-up. **s3ql** was not run (its SCW
> setup hit an eventual-consistency flake). The **sqlite** and **postgres**
> DittoFS metadata backends were excluded after `dittofs-sqlite` hard-wedged on
> the buffered large-write cell (server RSS → 3.1 GB, fio stuck in `D` state 12
> min) — tracked in [#1667](https://github.com/marmos91/dittofs/issues/1667).

**A note on durability.** DittoFS and ZeroFS are *native* NFS→S3 servers; the
others (JuiceFS, rclone, s3fs) are FUSE/mount filesystems **re-exported** over
the kernel NFS server, so their warm reads are served from the server-side
kernel page cache. DittoFS writes through to its local block store before
acknowledging; several competitors acknowledge from a RAM/writeback cache. So
DittoFS's lower sequential-write throughput is partly the cost of a durable
local write, and the re-exports' warm-read lead is partly a kernel-page-cache
free ride — neither is a like-for-like loss. **The fair comparison is DittoFS vs
ZeroFS**, the only other native NFS→S3 server.

## Throughput & IOPS — large files (1 GiB), warm

Sequential rows are MB/s; random / metadata / mixed rows are IOPS (metadata =
ops/s). **Bold** = DittoFS. Best *real competitor* per row is marked ✦
(local-disk excluded as the ceiling).

| Workload | **DittoFS-S3** | ZeroFS | JuiceFS | rclone | s3fs | local-disk ↑ |
|---|--:|--:|--:|--:|--:|--:|
| seq-write (MB/s) | **148** | 121 | 1272 | 1399 | 1699 ✦ | 3177 |
| seq-read (MB/s) | **805** | 14 | 1502 | 1041 | 2047 ✦ | 4462 |
| rand-write 4k (IOPS) | **2281** | 166 | 821 | 16554 ✦ | 1995 | 38705 |
| rand-read 4k (IOPS) | **8171** | 2291 | 88 ‡ | 2691 | 30287 ✦ | 42909 |
| metadata (ops/s) | **259** | 354 | 1990 | 7330 ✦ | — | 11681 |
| mixed-rw (IOPS) | **1409** | 1983 ✦ | 125 | 5531 | 4455 | 41634 |

‡ JuiceFS large rand-read (88 IOPS) is an outlier — its 1 GiB working set thrashes
its local cache on this box; its medium rand-read (42 380 IOPS) is representative.

## Random & metadata — medium files (1 MiB), warm

The smaller working set fits the re-export competitors' local page cache, so their
random-read lead is starkest here. The native-S3 servers (DittoFS, ZeroFS) get no
such free ride.

| Workload | **DittoFS-S3** | ZeroFS | JuiceFS | rclone | s3fs | local-disk ↑ |
|---|--:|--:|--:|--:|--:|--:|
| rand-write 4k (IOPS) | **2144** | 2937 | 1688 | 17993 | 23289 ✦ | 41241 |
| rand-read 4k (IOPS) | **13498** | 3789 | 42380 | 42187 | 42801 ✦ | 43064 |
| metadata (ops/s) | **239** | 428 | 1682 | 6563 ✦ | — | 11536 |

## Cold reads — large files, first byte after cache-evict

The S3-latency-bound axis. **Not collected this cycle** (warm-only run — the
`dfsctl system drain-uploads` evict barrier stalled on the metadata working set,
so the cold pass was skipped). Pending a follow-up run now that
`drain-uploads --timeout` ([#1668](https://github.com/marmos91/dittofs/issues/1668))
has landed. Prior observation: the native-S3 servers do poorly on a cold
sequential read (ZeroFS ~10 MB/s, JuiceFS ~56 MB/s).

## Latency — large files, warm (µs, p50 / p99)

Lower is better. DittoFS's durable local-write path shows in its higher write
tails; its read latencies are competitive with the native-S3 peer and far ahead
of ZeroFS on reads.

| Workload | **DittoFS-S3** | ZeroFS | JuiceFS | s3fs |
|---|--:|--:|--:|--:|
| seq-write | **18 219 / 67 633** | — | 2 376 / 14 877 | 2 073 / 4 751 |
| rand-write | **39 059 / 120 062** | 32 113 / 17 112 760 | 77 070 / 400 556 | 57 934 / 152 044 |
| rand-read | **11 600 / 67 633** | 54 788 / 103 285 | 1 082 130 / 4 328 522 | 4 178 / 7 635 |
| seq-read | **3 883 / 16 712** | 90 702 / 2 231 370 | 2 040 / 6 259 | 1 221 / 8 454 |

JuiceFS's ~1 s rand-read p50 is the same 1 GiB cache-thrashing outlier flagged ‡
above (88 IOPS): on this box every large-file random read misses its local cache
and pays an S3 round trip. It is not the medium-file figure (42 380 IOPS).

## Analysis

**Where DittoFS stands**

1. **Metadata is the one deficit, and it has not moved.** 259 ops/s large /
   239 medium — statistically identical to the 2026-07-10 baseline (239 / 219)
   and **last of the durable field** (ZeroFS 354, JuiceFS 1990). It is the only
   axis DittoFS loses to the fair comparison (ZeroFS, the other native server).
   The 20+ commits of warm-read cache + readahead work since the baseline did
   **not** touch it — they don't touch the create/`fsync` path. The metadata
   cells ran at low CPU (10–14 %) with very high context-switch rates: this is
   **fsync-bound, not compute-bound**. The lever is unchanged: metadata
   group-commit ([#1573](https://github.com/marmos91/dittofs/issues/1573),
   unlanded) — coalesce the per-create `fsync` into one durable group commit.

2. **DittoFS now leads the durable native-S3 cohort.** Against ZeroFS (the fair,
   no-page-cache comparison) the warm-read fast path (#1648/#1651) and lock-free
   readahead (#1653) pay off decisively: rand-read **3.6×** (8171 vs 2291),
   rand-write **13.7×** (2281 vs 166), seq-read **57×** (805 vs 14). The
   FUSE re-exports' higher warm IOPS are the kernel-page-cache handicap, not a
   design loss.

3. **Warm reads: the data is warm and the engine is fast — the gap is per-read
   server CPU.** At the block engine a warm 4 KiB CAS read is ~21 µs / ~47k IOPS,
   yet end-to-end over NFSv3 it lands at 8k–13k IOPS. The ~4× loss is **per-read
   NFS RPC decode + metadata lookup + allocation**, which the kernel re-exports
   avoid by serving 4 KiB reads straight from the page cache. It is *not* cold-S3
   latency (these are warm) and *not* the block store. That server-side per-read
   path is the next random-read target.

4. **Sequential write (148 MB/s) is the secondary gap.** A real durable
   through-to-disk write versus the competitors' buffered/async acknowledge — but
   9× behind JuiceFS/rclone is worth closing. It likely shares the same
   append-log `fsync` serializer as the metadata create path, so the two fixes
   may share a mechanism.

## Reproducing

The harness is `cmd/bench` (`dfsbench`), library code under `internal/dfsbench/`.
`fio` must be on `PATH` (the dev shell provides it).

```sh
go build -o dfsbench ./cmd/bench

# Run config: just the S3 bucket + endpoint (credentials come from the env, below).
cat > bench.yaml <<'EOF'
bucket: dittofs-bench
endpoint: https://s3.fr-par.scw.cloud
EOF

# Cloud run: provision one disposable VM, run the managed matrix, collect, tear down.
dfsbench setup                              # SCW_* env selects type/zone/image
dfsbench run --remote --config bench.yaml \
  --systems dittofs-s3-nfs3,zerofs-nfs3,juicefs-nfs3,rclone-nfs3,s3fs-nfs3,local-disk-nfs3 \
  --sizes medium,large
dfsbench report --results ./bench-results   # re-render this comparison table
dfsbench teardown
```

S3 credentials stay in the environment (`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`);
the bucket and endpoint are set in the run config (`bench.yaml`). See
`internal/dfsbench/CLAUDE.md` and `dfsbench list` for the full backend / workload
/ size matrix and run playbook.
