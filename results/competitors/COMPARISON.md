# Competitor Benchmark Comparison

_Date: 2026-03-06 | Hardware: Scaleway GP1-XS (16 GB RAM, 4 vCPU, 150 GB NVMe)_

## Test Parameters

| Parameter | Value |
|-----------|-------|
| Threads | 4 |
| File Size | 1 GiB (4 files = 4 GiB total) |
| Block Size | 4 KiB |
| Duration | 60s (random I/O) |
| Metadata Files | 1,000 |
| Small Files | 10,000 |
| Network | Same-region internal (< 1ms RTT) |
| Protocol | NFS (see notes per system) |
| Mount | `hard` (never `soft`) |

## Systems Under Test

| System | Type | Protocol | Storage Backend | Notes |
|--------|------|----------|-----------------|-------|
| **kernel-nfs** | Kernel | NFSv3 | Local filesystem | Linux knfsd — the gold standard |
| **ganesha** | Userspace | NFSv3 | Local filesystem | NFS-Ganesha with VFS FSAL |
| **rclone** | Userspace | NFSv3 | Local filesystem | rclone serve nfs |
| **samba** | Userspace | SMB2→NFS | Local filesystem | Samba over NFS re-export |
| **juicefs** | Userspace | NFSv3 | Local filesystem | JuiceFS with local Redis metadata + local storage |
| **dittofs** | Userspace | NFSv3 | BadgerDB + filesystem | DittoFS with 10 GiB cache |

## Results Summary

### Throughput & IOPS

| System | seq-write | seq-read | rand-write | rand-read | metadata | small-files |
|--------|-----------|----------|------------|-----------|----------|-------------|
| **kernel-nfs** | 49.2 MB/s | 63.9 MB/s | 1,446 IOPS | 2,317 IOPS | 341 ops/s | 419 ops/s |
| **ganesha** | 49.2 MB/s | 63.9 MB/s | 1,199 IOPS | 2,049 IOPS | 612 ops/s | 1,070 ops/s |
| **rclone** | 50.9 MB/s | 63.9 MB/s | 358 IOPS | 844 IOPS | 262 ops/s | 220 ops/s |
| **samba** | 31.0 MB/s | 43.5 MB/s | 773 IOPS | 881 IOPS | 115 ops/s | 282 ops/s |
| **juicefs** | 50.6 MB/s | 63.9 MB/s | 309 IOPS | 1,811 IOPS | 75 ops/s | 66 ops/s |
| **dittofs** | 16.6 MB/s | 63.9 MB/s | 331 IOPS | 636 IOPS | 367 ops/s | 58 ops/s |

### Latency (P50 / P95 / P99)

#### Sequential Write

| System | P50 | P95 | P99 |
|--------|-----|-----|-----|
| kernel-nfs | 630 us | 934 us | 1.5 ms |
| ganesha | 639 us | 949 us | 1.3 ms |
| rclone | 621 us | 800 us | 1.0 ms |
| samba | 499 us | 835 us | 11.9 ms |
| juicefs | 624 us | 848 us | 1.3 ms |
| dittofs | 612 us | 833 us | 996 us |

#### Sequential Read

| System | P50 | P95 | P99 |
|--------|-----|-----|-----|
| kernel-nfs | 16.0 ms | 20.5 ms | 214.6 ms |
| ganesha | 16.0 ms | 20.9 ms | 214.6 ms |
| rclone | 15.5 ms | 23.0 ms | 211.3 ms |
| samba | 96 us | 70.3 ms | 338.0 ms |
| juicefs | 16.2 ms | 20.5 ms | 213.9 ms |
| dittofs | 15.5 ms | 21.0 ms | 35.9 ms |

#### Random Write

| System | P50 | P95 | P99 |
|--------|-----|-----|-----|
| kernel-nfs | 676 us | 817 us | 947 us |
| ganesha | 819 us | 969 us | 1.1 ms |
| rclone | 2.3 ms | 2.6 ms | 3.0 ms |
| samba | 1.2 ms | 1.5 ms | 2.1 ms |
| juicefs | 1.4 ms | 2.6 ms | 3.4 ms |
| dittofs | 1.8 ms | 2.3 ms | 3.2 ms |

#### Random Read

| System | P50 | P95 | P99 |
|--------|-----|-----|-----|
| kernel-nfs | 391 us | 898 us | 1.0 ms |
| ganesha | 453 us | 945 us | 1.1 ms |
| rclone | 1.2 ms | 1.4 ms | 1.7 ms |
| samba | 614 us | 4.2 ms | 7.0 ms |
| juicefs | 452 us | 617 us | 1.2 ms |
| dittofs | 1.2 ms | 1.6 ms | 3.4 ms |

#### Metadata

| System | P50 | P95 | P99 |
|--------|-----|-----|-----|
| kernel-nfs | 2.6 ms | 6.6 ms | 8.6 ms |
| ganesha | 530 us | 4.5 ms | 6.7 ms |
| rclone | 1.6 ms | 15.1 ms | 24.1 ms |
| samba | 3.4 ms | 27.8 ms | 41.2 ms |
| juicefs | 9.4 ms | 26.6 ms | 70.6 ms |
| dittofs | 917 us | 11.2 ms | 15.9 ms |

#### Small Files

| System | P50 | P95 | P99 |
|--------|-----|-----|-----|
| kernel-nfs | 9.7 ms | 19.5 ms | 23.6 ms |
| ganesha | 3.7 ms | 8.2 ms | 11.3 ms |
| rclone | 15.0 ms | 37.4 ms | 46.3 ms |
| samba | 12.1 ms | 34.0 ms | 47.4 ms |
| juicefs | 41.4 ms | 143.4 ms | 572.9 ms |
| dittofs | 10.4 ms | 287.6 ms | 449.9 ms |

## Relative Performance vs kernel-nfs

| System | seq-write | seq-read | rand-write | rand-read | metadata | small-files |
|--------|-----------|----------|------------|-----------|----------|-------------|
| **kernel-nfs** | baseline | baseline | baseline | baseline | baseline | baseline |
| **ganesha** | 1.00x | 1.00x | 0.83x | 0.88x | 1.80x | 2.55x |
| **rclone** | 1.03x | 1.00x | 0.25x | 0.36x | 0.77x | 0.52x |
| **samba** | 0.63x | 0.68x | 0.53x | 0.38x | 0.34x | 0.67x |
| **juicefs** | 1.03x | 1.00x | 0.21x | 0.78x | 0.22x | 0.16x |
| **dittofs** | **0.34x** | 1.00x | 0.23x | 0.27x | 1.08x | 0.14x |

## Analysis

### Where DittoFS Wins

1. **Sequential read** — Network-capped at 63.9 MB/s, identical to all NFS-based systems. Cache is effective for reads.
2. **Metadata** — 367 ops/s, faster than kernel-nfs (341), rclone (262), samba (115), and juicefs (75). Only Ganesha is faster (612).
3. **Per-op write latency** — P50 612 us is the lowest of all systems for seq-write (good cache absorption).

### Where DittoFS Loses (Critical)

1. **Sequential write: 16.6 MB/s (0.34x kernel-nfs)** — This is the biggest problem. Every other userspace system achieves ~50 MB/s. DittoFS is 3x slower. This is NOT network-bound (all others hit ~50 MB/s on the same wire). The per-op latency is good (612 us P50), which means the throughput bottleneck is likely in how blocks are committed/flushed, not in individual write handling.

2. **Random read: 636 IOPS (0.27x kernel-nfs)** — Worst of all systems. Even rclone (844) and samba (881) beat DittoFS here. Cache miss path is too slow — likely due to the cache lookup + BadgerDB overhead per 4 KiB read.

3. **Random write: 331 IOPS (0.23x kernel-nfs)** — Close to rclone (358) and juicefs (309), but far behind kernel-nfs (1,446), ganesha (1,199), and samba (773). Cache write overhead per 4 KiB block is too high.

4. **Small files: 58 ops/s (0.14x kernel-nfs)** — Dead last. Even juicefs (66) edges ahead. The P95 latency spike to 287 ms suggests periodic cache flushes or offloader stalls blocking small-file creation.

### Key Observations

- **The seq-write anomaly is the #1 priority.** The low per-op latency (612 us P50, lowest of all) combined with low throughput (16.6 MB/s) means DittoFS is writing each 4 KiB block quickly but not pushing enough data per second. This points to serialization or blocking in the write pipeline — possibly the offloader or WAL sync path throttling throughput despite good per-op latency.

- **The random I/O gap is structural.** Every operation goes through cache → BadgerDB → filesystem, adding overhead that kernel-nfs and ganesha avoid entirely (they write directly to VFS). This is an expected cost of the abstraction layer, but the gap should be narrower.

- **Small-files P95 spike (287 ms)** strongly suggests the offloader or cache eviction path blocks foreground operations periodically.

### Competitive Standing

| Workload | Beat userspace competitors? | Verdict |
|----------|-----------------------------|---------|
| seq-write | NO — worst by far (16.6 vs 49-51 MB/s) | MUST FIX |
| seq-read | TIE — all network-capped | OK |
| rand-write | ~TIE with rclone/juicefs, loses to ganesha/samba | Acceptable |
| rand-read | NO — worst overall | SHOULD FIX |
| metadata | YES — beats rclone/samba/juicefs | GOOD |
| small-files | NO — worst overall | SHOULD FIX |
