# DittoFS Benchmark Report — Round 15

_Date: 2026-03-07_
_Branch: `feat/cache-rewrite` @ latest_

## Configuration

| Parameter | Value |
|-----------|-------|
| Server | Scaleway GP1-XS (4 vCPU, 16GB RAM, NVMe) |
| Client | Scaleway GP1-XS (separate instance) |
| Network | Private LAN (same AZ) |
| NFS Version | NFSv3, hard mount |
| Mount Options | `rsize=1048576,wsize=1048576` |
| Threads | 4 |
| File Size | 1 GiB |
| Block Size | 4 KiB |
| Duration | 60s per workload |
| Metadata Store | BadgerDB |

## Results

### DittoFS — Filesystem Backend

| Workload | Throughput | IOPS | Ops/s | P50 | P95 | P99 |
|----------|-----------|------|-------|-----|-----|-----|
| seq-write | 50.9 MB/s | - | - | 620 us | 797 us | 953 us |
| seq-read | 63.9 MB/s | - | - | 13.2 ms | 20.4 ms | 216.3 ms |
| rand-write | - | 281 | - | 2.0 ms | 4.6 ms | 23.5 ms |
| rand-read | - | 655 | - | 1.2 ms | 1.7 ms | 3.9 ms |
| metadata | - | - | 597 | 987 us | 5.4 ms | 8.1 ms |

### DittoFS — S3 Backend (Scaleway Object Storage)

| Workload | Throughput | IOPS | Ops/s | P50 | P95 | P99 |
|----------|-----------|------|-------|-----|-----|-----|
| seq-write | 50.9 MB/s | - | - | 612 us | 805 us | 1.1 ms |
| seq-read | 64.0 MB/s | - | - | 14.6 ms | 20.4 ms | 215.2 ms |
| rand-write | - | 308 | - | 1.8 ms | 2.2 ms | 8.7 ms |
| rand-read | - | 594 | - | 1.3 ms | 1.7 ms | 5.2 ms |
| metadata | - | - | 486 | 978 us | 7.4 ms | 12.2 ms |

## Competitor Comparison

All competitors tested on the same infrastructure with identical mount options.

| Workload | DittoFS (fs) | DittoFS (S3) | kernel-nfs | ganesha | rclone | samba | juicefs |
|----------|-------------|-------------|------------|---------|--------|-------|---------|
| seq-write | **50.9 MB/s** | **50.9 MB/s** | 49.2 MB/s | 49.2 MB/s | 50.9 MB/s | 31.0 MB/s | 50.6 MB/s |
| seq-read | 63.9 MB/s | 64.0 MB/s | 63.9 MB/s | 63.9 MB/s | 63.9 MB/s | 43.5 MB/s | 63.9 MB/s |
| rand-write | 281 IOPS | 308 IOPS | 1,446 IOPS | 1,199 IOPS | 358 IOPS | 773 IOPS | 309 IOPS |
| rand-read | 655 IOPS | 594 IOPS | 2,317 IOPS | 2,049 IOPS | 844 IOPS | 881 IOPS | 1,811 IOPS |
| metadata | **597 ops/s** | 486 ops/s | 341 ops/s | 612 ops/s | 262 ops/s | 115 ops/s | 75 ops/s |

### Analysis

**DittoFS wins or ties:**
- **seq-write**: 50.9 MB/s — matches or beats all competitors (network-limited)
- **seq-read**: 63.9-64.0 MB/s — tied with all (network-limited)
- **metadata (fs)**: 597 ops/s — beats kernel-nfs (341), rclone (262), samba (115), juicefs (75); close to ganesha (612)

**DittoFS competitive:**
- **rand-write (S3)**: 308 IOPS — matches juicefs (309), close to rclone (358)
- **metadata (S3)**: 486 ops/s — beats kernel-nfs, rclone, samba, juicefs

**DittoFS trailing:**
- **rand-write (fs)**: 281 IOPS — below rclone (358) and juicefs (309). Root cause: each 4KB random write requires BadgerDB metadata update + cache management overhead
- **rand-read**: 594-655 IOPS — below kernel-nfs (2,317) and ganesha (2,049). Root cause: FileBlockStore metadata lookup per block read

### Fixes Applied (since Round 12)

1. **Eager `.blk` file creation** — `tryDirectDiskWrite` creates cache files on first write for writes >= 64KiB
2. **`fb-sealed:` BadgerDB index** — O(sealed) upload scan instead of O(all) full-table scan
3. **Channel-based buffer pool** — Avoids GC madvise churn from sync.Pool on 8MB pages
4. **map+RWMutex** — Replaces sync.Map which degraded under high key churn
5. **fsync deferred to COMMIT** — Removed from write hot path, only on NFS COMMIT
6. **dropPageCache removed** — Kernel reclaims pages naturally
7. **diskUsed accounting fix** — Correct delta tracking on block re-flush
