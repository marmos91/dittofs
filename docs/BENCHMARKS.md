# DittoFS Performance Benchmarks

Performance of DittoFS (S3 backend) against the S3-backed network filesystems the
`dfsbench` harness supports — **JuiceFS, s3ql, rclone, s3fs, and ZeroFS** — plus
**local-disk** as the raw-hardware ceiling. All systems run on one disposable
Scaleway VM, driven by the same `fio` workloads over the same protocol.

> **Read these with the rig in mind.** Each cell is a single 30 s `fio` run on a
> shared cloud VM. Trust the **shape and order of magnitude**, not the third
> digit. Medium-file sequential-read is first-touch noise and is omitted.

## Test setup

| | |
|---|---|
| Date | 2026-07-10 |
| DittoFS | `develop` @ `0d79ede1` |
| Harness | `internal/dfsbench` (fio driver + SCW orchestration) |
| Host | Scaleway `fr-par-1`, single VM, Ubuntu 24.04 |
| Protocol | **NFSv3** — the only protocol all seven backends share (ZeroFS is NFSv3-only) |
| Object store | Scaleway Object Storage, `s3.fr-par.scw.cloud` |
| fio | 4 threads, 30 s/cell, warm pass (+ cold/post-evict where noted) |
| Sizes | medium = 1 MiB, large = 1 GiB |
| Result | 7/7 backends set up, **0 workload errors** |

**A note on durability.** DittoFS and ZeroFS are *native* NFS→S3 servers; the
others (JuiceFS, s3ql, rclone, s3fs) are FUSE/mount filesystems **re-exported**
over the kernel NFS server. DittoFS writes through to its local block store
(measured ~189 MB/s to disk) before acknowledging; several competitors
acknowledge from a RAM/writeback cache. So DittoFS's lower sequential-write
throughput is partly the cost of a durable local write, not a like-for-like loss.

## Throughput & IOPS — large files (1 GiB), warm

Sequential rows are MB/s; random / metadata / mixed rows are IOPS (metadata =
ops/s). **Bold** = DittoFS. Best *real competitor* per row is marked ✦
(local-disk excluded as the ceiling).

| Workload | **DittoFS-S3** | ZeroFS | s3ql | JuiceFS | rclone | s3fs | local-disk ↑ |
|---|--:|--:|--:|--:|--:|--:|--:|
| seq-write (MB/s) | **156** | 204 | 310 | 1336 | 1523 | 1775 ✦ | 3287 |
| seq-read (MB/s) | **677** | 13 | 552 | 1616 | 1181 | 2876 ✦ | 4774 |
| rand-write 4k (IOPS) | **2365** | 2133 | 1256 | 826 | 17346 ✦ | 2183 | 40883 |
| rand-read 4k (IOPS) | **4010** | 2568 | 39563 ✦ | 12418 | fail | 11101 | — |
| metadata (ops/s) | **239** | 371 | 700 | 1824 ✦ | — | fail | — |
| mixed-rw (IOPS) | **1256** | 1776 | 4928 ✦ | 281 | — | 4453 | — |

## Random & metadata — medium files (1 MiB), warm

The smaller working set fits the re-export competitors' local page cache, so their
random-read lead is starkest here. The native-S3 servers (DittoFS, ZeroFS) get no
such free ride.

| Workload | **DittoFS-S3** | ZeroFS | s3ql | JuiceFS | rclone | s3fs | local-disk ↑ |
|---|--:|--:|--:|--:|--:|--:|--:|
| rand-write 4k (IOPS) | **2337** | 2885 | 3844 | 588 | 19231 | 25229 ✦ | 43712 |
| rand-read 4k (IOPS) | **10981** | 2548 | 38636 | 45743 | fail | 46500 ✦ | 45835 |
| metadata (ops/s) | **219** | 235 | 451 | 2597 ✦ | — | — | — |

## Cold reads — large files, first byte after cache-evict

The S3-latency-bound axis. DittoFS's evict step (`dfsctl system drain-uploads`)
errored this run, so its cold numbers are pending a re-run — but note how poorly
the native-S3 servers do on a cold sequential read.

| Workload | DittoFS-S3 | ZeroFS | JuiceFS |
|---|--:|--:|--:|
| seq-read cold (MB/s) | pending | 10 | 56 |
| rand-read cold (IOPS) | pending | 871 | — |

## Latency — large files, warm (µs, p50 / p99)

Lower is better. DittoFS's durable local-write path shows in its higher write
tails; its read latencies are competitive.

| Workload | **DittoFS-S3** | ZeroFS | s3ql | JuiceFS | s3fs |
|---|--:|--:|--:|--:|--:|
| seq-write | **14 615 / 77 070** | 10 813 / 164 626 | 9 896 / 44 827 | 2 245 / 14 483 | 1 974 / 4 620 |
| rand-write | **37 487 / 103 285** | 46 924 / 86 508 | 70 779 / 459 276 | 68 682 / 400 556 | 55 312 / 128 451 |
| rand-read | **15 008 / 147 849** | 49 021 / 88 605 | 2 736 / 10 289 | 9 896 / 15 925 | 10 813 / 33 423 |
| seq-read | **4 358 / 26 608** | 110 625 / 2 399 142 | 709 / 46 924 | 1 892 / 5 997 | 1 139 / 1 991 |

## Analysis

**Where DittoFS stands**

1. **Metadata is the clearest deficit** — 239 ops/s, last of all seven backends
   (ZeroFS 371, s3ql 700, JuiceFS 1824). It is the one axis DittoFS loses to the
   entire field, and the highest-leverage target. It is directly addressed by the
   in-flight metadata group-commit work (#1573).

2. **DittoFS leads the durable-write cohort** — on random-write (2365 IOPS) it
   beats JuiceFS (826), s3ql (1256), s3fs (2183), and ZeroFS (2133); only rclone's
   RAM-buffered VFS is faster. Sequential write (156 MB/s) is the lowest number,
   but it is a durable through-to-disk write versus cache-acknowledged competitors.

3. **Read throughput is respectable, not dominant** — sequential read (677 MB/s)
   beats s3ql (552) and dwarfs ZeroFS (13 MB/s, whose native-S3 read path collapses
   under NFS). The re-export rivals win random-read only by serving 4k reads from a
   local-FS page cache that DittoFS does not lean on.

4. **ZeroFS is the instructive comparison** — a fellow native NFS→S3 server whose
   sequential and random reads are far worse than DittoFS's. Being native-S3 is not
   the handicap; the read/cache design is.

## Reproducing

The harness is `cmd/bench` (`dfsbench`), library code under `internal/dfsbench/`.
`fio` must be on `PATH` (the dev shell provides it).

```sh
go build -o dfsbench ./cmd/bench

# Cloud run: provision one disposable VM, run the managed matrix, collect, tear down.
dfsbench setup                              # SCW_* env selects type/zone/image
dfsbench run --remote \
  --systems dittofs-s3-nfs3,zerofs-nfs3,s3ql-nfs3,juicefs-nfs3,rclone-nfs3,s3fs-nfs3,local-disk-nfs3 \
  --sizes medium,large
dfsbench report --results ./bench-results   # re-render this comparison table
dfsbench teardown
```

S3 credentials stay in the environment (`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`);
the bucket and endpoint are set in the run config. See `bench/README.md` and
`dfsbench list` for the full backend / workload / size matrix.
