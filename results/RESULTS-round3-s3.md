# Benchmark Results — Round 3 (S3-Backed, NFS3)

**Date**: 2026-03-03
**Infrastructure**: Scaleway PLAY2-MICRO (2 vCPU, 8 GB RAM, 8 GB local NVMe)
**Setup**: Client VM (51.15.199.235) -> NFS3 -> Server VM (212.47.241.220) -> Scaleway Object Storage (fr-par)
**Parameters**: 4 threads, 64 MiB files, 30s duration per workload, 200 metadata files
**DittoFS branch**: `feat/benchmark-infrastructure` (commit 16abe48, includes backpressure fix + cache alignment)

## Cache Alignment

All systems are configured with a **uniform 2GB local cache** to ensure apple-to-apple
comparison. In Round 2, cache sizes were inconsistent (rclone: unlimited, s3ql: autodetect
up to 10GB, juicefs: 1GB), which skewed results in favor of systems with larger caches.

| System | Cache Setting | Size |
|--------|--------------|------|
| DittoFS | `cache.size: "2GB"` | 2 GB |
| Rclone-S3 | `--vfs-cache-max-size 2G` | 2 GB |
| JuiceFS-S3 | `--cache-size 2048` (MiB) | 2 GB |
| S3QL | `--cachesize 2097152` (KiB) | 2 GB |

## Raw S3 Baseline

Measured with rclone from the server VM to Scaleway Object Storage:

| Operation | Throughput |
|-----------|-----------|
| Multi-file PUT (50x4MB, 8 parallel) | ~47 MB/s |
| Multi-file GET (50x4MB, 4 parallel) | ~263 MB/s |
| Large file PUT (256MB, 64M chunks) | ~47 MB/s |
| Large file GET (256MB) | ~36 MB/s |
| HEAD latency | ~201 ms |
| LIST latency | ~159 ms |

## Systems Under Test

| System | Backend | NFS Implementation | Notes |
|--------|---------|-------------------|-------|
| **DittoFS** (badger-s3-nfs3) | BadgerDB metadata + S3 payload | Pure Go userspace NFSv3 | Cache 2GB, backpressure, 16 parallel uploads |
| **Rclone-S3** | VFS cache + S3 | Pure Go userspace NFSv3 (go-nfs) | `--vfs-cache-mode writes`, 2GB cache cap |
| **JuiceFS-S3** | SQLite metadata + S3 data | FUSE mount re-exported via knfsd | JuiceFS CE, 2GB cache |
| **S3QL** | SQLite metadata + S3 data | FUSE mount re-exported via knfsd | S3QL v5, 2GB cache cap |

## Sequential I/O

| Metric | DittoFS | Rclone-S3 | JuiceFS-S3 | S3QL |
|--------|---------|-----------|------------|------|
| **seq-write** (MB/s) | 234 | 291 | 24 | **331** |
| seq-write p50 | 613 us | 622 us | 761 us | 705 us |
| seq-write p99 | 893 us | 897 us | 1612 us | 967 us |
| **seq-read** (MB/s) | 285 | 333 | 142 | **629** |
| seq-read p50 | 3160 us | 2488 us | 1751 us | 1301 us |
| seq-read p99 | 11731 us | 7226 us | 117175 us | 3670 us |

**Analysis**:
- All systems except JuiceFS vastly exceed the raw S3 write ceiling (~47 MB/s) — local caching absorbs writes before S3 upload.
- **S3QL leads** at 331 MB/s write / 629 MB/s read, benefiting from aggressive FUSE-level caching.
- **DittoFS at 234/285 MB/s** is competitive. Writes are ~80% of rclone, reads ~86% of rclone.
- **JuiceFS is bottlenecked by sync flush** (24 MB/s write) — every write waits for S3 ACK.
- Read throughput above raw S3 ceiling means cache is serving previously-written data.

## Random I/O

| Metric | DittoFS | Rclone-S3 | JuiceFS-S3 | S3QL |
|--------|---------|-----------|------------|------|
| **rand-write** (IOPS) | 99 | 970 | 4,456 | **51,870** |
| rand-write p50 | 8644 us | 1270 us | 4 us | 3 us |
| rand-write p99 | 22786 us | 1991 us | 2845 us | 585 us |
| **rand-read** (IOPS) | **298,053** | 117,157 | 156,836 | 298,731 |
| rand-read p50 | 1 us | 1 us | 1 us | 1 us |
| rand-read p99 | 2 us | 14 us | 13 us | 10 us |

**Analysis**:
- **rand-read**: DittoFS ties S3QL at ~298K IOPS — cache is serving all reads at memory speed. Both systems keep previously-written data hot in cache.
- **rand-write (99 IOPS)**: DittoFS's weakest point. The backpressure mechanism blocks writers when pending S3 uploads fill the 1GB limit. Each random 4KB write dirties a full 4MB block, creating massive write amplification. S3QL (51K IOPS) and JuiceFS (4.5K IOPS) buffer writes purely in local cache/FUSE without blocking on S3 upload.
- rclone's 970 IOPS comes from its VFS write-back cache absorbing random writes.

## Metadata Operations

| Metric | DittoFS | Rclone-S3 | JuiceFS-S3 | S3QL |
|--------|---------|-----------|------------|------|
| **metadata** (ops/sec) | 252 | 779 | 10 | **1,029** |
| metadata p50 | 2391 us | 429 us | 6549 us | 646 us |
| metadata p99 | 22811 us | 3821 us | 1047660 us | 2927 us |

**Analysis**:
- **S3QL leads** at 1029 ops/s — its SQLite metadata is fully local, no S3 round-trips for metadata.
- **Rclone at 779 ops/s** — VFS cache handles metadata locally.
- **DittoFS at 252 ops/s** — slower than local-FS round (1018 ops/s). The `small_file_threshold` setting forces sync flush for metadata test files (11 bytes each), adding S3 latency per operation.
- **JuiceFS at 10 ops/s** — sync flush to S3 on every metadata change.

## Summary

| Category | Winner | DittoFS Rank | DittoFS Value | vs. Winner |
|----------|--------|-------------|---------------|------------|
| seq-write | S3QL (331 MB/s) | 3rd | 234 MB/s | 71% |
| seq-read | S3QL (629 MB/s) | 3rd | 285 MB/s | 45% |
| rand-write | S3QL (51,870 IOPS) | 4th | 99 IOPS | 0.2% |
| rand-read | S3QL (298,731 IOPS) | 1st (tied) | 298,053 IOPS | ~100% |
| metadata | S3QL (1,029 ops/s) | 3rd | 252 ops/s | 24% |

### Key Takeaways

1. **Backpressure fix works** — DittoFS completes all benchmarks without crashes or cache-full errors
2. **rand-read is excellent** — 298K IOPS, tied for #1, cache is highly effective for reads
3. **Sequential I/O is solid** — 234/285 MB/s, well above raw S3 ceiling, competitive with rclone
4. **rand-write is the critical weakness** — 99 IOPS due to backpressure blocking on S3 uploads. The 4MB block size means every 4KB random write dirties a full block, creating ~1000x write amplification
5. **Metadata regressed from local-FS** — 252 vs 1018 ops/s. The `small_file_threshold` sync flush adds S3 latency; needs tuning or disabling for metadata-heavy workloads
6. **S3QL wins across the board** — its FUSE + aggressive local caching strategy defers all S3 I/O, making it appear fast at the cost of data durability (unflushed writes lost on crash)

### DittoFS Performance Bottlenecks (for deep dive)

- **rand-write**: Backpressure blocks on S3 upload. Fix options: larger `max_pending_size`, lazy flush, dirty block coalescing, sub-block writes
- **metadata**: `small_file_threshold` forces sync flush for tiny files. Fix: exempt metadata test files from S3 flush, or increase threshold
- **seq-read latency**: p50=3160us vs s3ql's 1301us — possible prefetch or read-ahead gap
- **seq-write throughput**: 234 vs 331 MB/s — cache write path overhead vs. FUSE's direct memory buffer
