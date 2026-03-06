# Round 2 Results — S3-Backed Filesystems

**Date**: 2026-03-03
**Infrastructure**: Scaleway DEV1-S (2 vCPU, 2GB RAM), fr-par region
**S3 Provider**: Scaleway Object Storage (fr-par), bucket `dittofs-bench`
**Benchmark Config**: 64 MiB files, 4 KiB blocks, 30s duration, 4 threads, 200 metadata files

## Raw S3 Baseline

Direct S3 PUT/GET from the server VM (no filesystem layer), using rclone as client:

| Operation | Throughput | Notes |
|-----------|-----------|-------|
| Multi-file PUT (50x4MB) | 25-47 MB/s | Best at 8 transfers |
| Multi-file GET (50x4MB) | 118-263 MB/s | Best at 4 transfers |
| Large file PUT (256MB) | 18-47 MB/s | Best with 64M chunks, 8 concurrency |
| Large file GET (256MB) | 35.6 MB/s | Single stream |
| HEAD latency | 201 ms avg | |
| LIST latency | 159 ms avg | |

**Key takeaway**: Scaleway S3 write ceiling is ~47 MB/s, read ceiling is ~263 MB/s (multi-file) or ~36 MB/s (single large file). Any filesystem reporting higher sustained throughput is serving from local cache.

## S3-Backed Filesystem Benchmarks

| Metric | Rclone-S3 | JuiceFS-S3 | S3QL |
|--------|-----------|------------|------|
| seq-write | 290.6 MB/s | 23.9 MB/s | **331.0 MB/s** |
| seq-read | 332.7 MB/s | 142.4 MB/s | **629.4 MB/s** |
| rand-write | 970 IOPS | 4,456 IOPS | **51,870 IOPS** |
| rand-read | 117,157 IOPS | 156,836 IOPS | **298,731 IOPS** |
| metadata | 779 ops/s | 10 ops/s | **1,029 ops/s** |

## Analysis

### Caching Dominates Everything

The raw S3 write throughput is ~47 MB/s, yet rclone-S3 reports 291 MB/s seq-write and S3QL reports 331 MB/s. This means **these benchmarks primarily measure local caching**, not S3 performance.

- **S3QL**: Most aggressive caching — FUSE mount with large local cache + kernel NFS re-export. The NFS client adds another cache layer. Result: 7x the raw S3 write speed.
- **Rclone-S3**: VFS cache in `writes` mode buffers writes locally, uploads asynchronously. 6x raw S3 speed.
- **JuiceFS-S3**: 24 MB/s seq-write — this is the only system actually bottlenecked by S3 uploads during writes. Honest but slow.

### Random I/O is Pure Cache

Random read IOPS (100K-300K) are impossible over S3 — these are 100% from local cache. The 64MiB test files fit entirely in the 2GB VM RAM, so after sequential write, all data is cached.

### Metadata: S3 is the Bottleneck

With HEAD latency at 201ms, any S3-backed system faces fundamental metadata limitations. Yet S3QL achieves 1029 ops/s — this is because metadata is in a local SQLite database (s3ql stores metadata separately from data). JuiceFS also uses SQLite but only achieves 10 ops/s, suggesting it's synchronizing metadata to S3 on every operation.

### Per-System Notes

**Rclone-S3** (rclone serve nfs → S3)
- Good sequential I/O through VFS write caching
- Weak rand-write (970 IOPS) — likely no write-back cache for random patterns
- Simple architecture, no FUSE layer

**JuiceFS-S3** (FUSE mount + kernel NFS re-export → SQLite + S3)
- Surprisingly poor: 24 MB/s writes, 10 ops/s metadata
- JuiceFS appears to be flushing to S3 synchronously
- The FUSE+NFS double-hop doesn't help

**S3QL** (FUSE mount + kernel NFS re-export → S3)
- Wins every metric by a wide margin
- Heavy local caching makes it effectively a local filesystem for small working sets
- Best for read-heavy workloads where data fits in cache
- Production concern: cache consistency and durability

## DittoFS S3 — Not Tested

DittoFS with S3 payload backend was **not benchmarked** due to a cache-full bug:
when S3 upload is slower than NFS write rate, the cache fills up and rejects writes with
"cache full: pending data cannot be evicted" instead of applying backpressure.
This needs to be fixed before S3 benchmarks can run.

## Comparison with Local-FS Results (Round 2)

For reference, the local filesystem results from the same VMs:

| Metric | DittoFS NFS3 | Kernel NFS | Rclone (local) | JuiceFS (local) |
|--------|-------------|------------|----------------|-----------------|
| seq-write | 67.0 MB/s | 61.1 | 66.5 | 65.7 |
| seq-read | 67.7 | 67.4 | 67.7 | 67.9 |
| rand-write | 1,464 IOPS | 1,972 | 752 | 689 |
| rand-read | 473 | 5,579 | 582 | 1,792 |
| metadata | 1,018 ops/s | 367 | 848 | 121 |

The S3-backed numbers for rclone and S3QL *exceed* the local-FS numbers because local-FS was bottlenecked by the VM's 200Mbit network link (~25 MB/s per stream), while S3-backed systems cache writes locally and only flush to S3 asynchronously.
