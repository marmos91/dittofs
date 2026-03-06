# Benchmark Results — Round 2 (Local FS, NFS3)

**Date**: 2026-03-02
**Infrastructure**: Scaleway PLAY2-MICRO (2 vCPU, 8 GB RAM, 8 GB local NVMe)
**Setup**: Client VM (51.15.199.235) -> NFS3 -> Server VM (212.47.241.220)
**Parameters**: 4 threads, 256 MiB files, 30s duration per workload, 1000 metadata files
**DittoFS branch**: `feat/benchmark-infrastructure` (commit a1c231e, includes mtime fix + FADV_DONTNEED)

## Systems Under Test

| System | Backend | NFS Implementation | Notes |
|--------|---------|-------------------|-------|
| **DittoFS** (badger-fs-nfs3) | BadgerDB metadata + local FS payload | Pure Go userspace NFSv3 | Our system |
| **Kernel NFS** | Local filesystem (ext4) | Linux knfsd (kernel) | Gold standard baseline |
| **Rclone** | Local filesystem + VFS cache | Pure Go userspace NFSv3 (go-nfs) | `rclone serve nfs --vfs-cache-mode writes` |
| **JuiceFS** | SQLite metadata + local file storage | FUSE mount re-exported via knfsd | JuiceFS CE 1.2.2 |

## Sequential I/O

| Metric | DittoFS | Kernel NFS | Rclone | JuiceFS |
|--------|---------|------------|--------|---------|
| **seq-write** (MB/s) | **67.0** | 61.1 | 66.5 | 65.7 |
| seq-write p50 | 673 us | 659 us | 655 us | 703 us |
| seq-write p99 | 1508 us | 1071 us | 947 us | 1019 us |
| **seq-read** (MB/s) | 67.7 | 67.4 | 67.7 | **67.9** |
| seq-read p50 | 3514 us | 1364 us | 2647 us | 1751 us |
| seq-read p99 | 225.8 ms | 229.0 ms | 220.4 ms | 223.1 ms |

**Analysis**: Sequential throughput is virtually identical across all systems (~67 MB/s), bottlenecked by the PLAY2-MICRO network/disk. DittoFS matches kernel NFS here.

## Random I/O

| Metric | DittoFS | Kernel NFS | Rclone | JuiceFS |
|--------|---------|------------|--------|---------|
| **rand-write** (IOPS) | **1464** | 1972 | 752 | 689 |
| rand-write p50 | 745 us | 524 us | 1389 us | 1324 us |
| rand-write p99 | 1162 us | 908 us | 2079 us | 3779 us |
| **rand-read** (IOPS) | 473 | **5579** | 582 | 1792 |
| rand-read p50 | 1854 us | 237 us | 1013 us | 472 us |
| rand-read p99 | 6807 us | 557 us | 14429 us | 2526 us |

**Analysis**:
- **rand-write**: DittoFS is 2nd best at 1464 IOPS (74% of kernel NFS). Both rclone and juicefs are ~2x slower.
- **rand-read**: Kernel NFS dominates at 5579 IOPS (with FADV_DONTNEED active). JuiceFS benefits from its local cache layer. DittoFS at 473 IOPS needs investigation — potential cache inefficiency.

## Metadata Operations

| Metric | DittoFS | Kernel NFS | Rclone | JuiceFS |
|--------|---------|------------|--------|---------|
| **metadata** (ops/sec) | **1018** | 367 | 848 | 121 |
| metadata p50 | 554 us | 2452 us | 483 us | 7289 us |
| metadata p99 | 3066 us | 8522 us | 3574 us | 26488 us |

**Analysis**: DittoFS leads significantly in metadata performance — **2.8x faster than kernel NFS** and **8.4x faster than JuiceFS**. This is DittoFS's strongest advantage: the BadgerDB metadata store handles create/stat/delete operations very efficiently over NFS.

## Summary

| Category | Winner | DittoFS Rank |
|----------|--------|--------------|
| seq-write | DittoFS (67.0 MB/s) | 1st |
| seq-read | All tied (~67.7 MB/s) | Tied |
| rand-write | Kernel NFS (1972) | 2nd (74%) |
| rand-read | Kernel NFS (5579) | 4th (8.5%) |
| metadata | DittoFS (1018) | 1st (2.8x kernel) |

### Key Takeaways

1. **DittoFS is competitive on sequential I/O** — matches or beats kernel NFS on write, ties on read
2. **Metadata is DittoFS's killer feature** — nearly 3x faster than kernel NFS
3. **Random read needs work** — 473 IOPS vs kernel's 5579 suggests cache bypass isn't optimal
4. **Random write is solid** — 74% of kernel NFS, 2x better than rclone/juicefs
5. **All systems hit the same network/disk ceiling** on sequential ops (~67 MB/s)

### Next Steps

- [ ] Provision Scaleway S3 buckets for S3-backed benchmarks (primary use case)
- [ ] Run dittofs-badger-s3, rclone+S3, juicefs+S3, s3ql benchmarks
- [ ] Investigate rand-read performance gap
- [ ] Investigate NFSv4.1 hang during rand-write
- [ ] Test on larger VMs (20GB+ disk, more CPU/RAM)
