# Round 24 Benchmark Results (2026-03-09)

## Configuration
- Duration: 60s per workload
- Threads: 4
- File size: 1 GiB
- Block size: 4 KiB
- Meta files: 1,000
- Small file count: 10,000
- Server: 51.15.211.189 (GP1-XS, 16GB, 4 vCPU)
- Client: 51.15.199.235
- Cache: 4GB on server

## Results

| Workload | DittoFS S3 NFSv3 | DittoFS S3 NFSv4.1 | JuiceFS S3 | kernel-nfs (local FS baseline) |
|---|---|---|---|---|
| seq-write | 50.8 MB/s | 50.7 MB/s | 31.2 MB/s | 49.2 MB/s |
| seq-read | 63.9 MB/s | 63.9 MB/s | 50.5 MB/s | 63.9 MB/s |
| rand-write | 634 IOPS | 635 IOPS | 60 IOPS | 1,234 IOPS |
| rand-read | 1,383 IOPS | 1,420 IOPS | 1,447 IOPS | 2,241 IOPS |
| metadata | 146 ops/s | 609 ops/s | 7 ops/s | 290 ops/s |
| small-files | 154 ops/s | 1,792 ops/s | 44 ops/s | 492 ops/s |

## DittoFS S3 NFSv4.1 vs JuiceFS S3

| Workload | DittoFS | JuiceFS | Ratio |
|---|---|---|---|
| seq-write | 50.7 MB/s | 31.2 MB/s | **1.6x** |
| seq-read | 63.9 MB/s | 50.5 MB/s | **1.3x** |
| rand-write | 635 IOPS | 60 IOPS | **10.6x** |
| rand-read | 1,420 IOPS | 1,447 IOPS | 0.98x |
| metadata | 609 ops/s | 7 ops/s | **87x** |
| small-files | 1,792 ops/s | 44 ops/s | **41x** |

## DittoFS S3 NFSv4.1 vs kernel-nfs (local disk)

| Workload | DittoFS S3 | kernel-nfs | % of kernel |
|---|---|---|---|
| seq-write | 50.7 MB/s | 49.2 MB/s | **103%** |
| seq-read | 63.9 MB/s | 63.9 MB/s | **100%** |
| rand-write | 635 IOPS | 1,234 IOPS | 51% |
| rand-read | 1,420 IOPS | 2,241 IOPS | 63% |
| metadata | 609 ops/s | 290 ops/s | **210%** |
| small-files | 1,792 ops/s | 44 ops/s | **364%** |

## Key Observations

1. **DittoFS S3 crushes JuiceFS** on every workload except rand-read (tied)
2. **DittoFS S3 beats kernel-nfs** on seq-write, metadata, and small-files
3. **NFSv4.1 >> NFSv3** for metadata (609 vs 146) and small-files (1,792 vs 154) on DittoFS
4. rand-write and rand-read are lower than kernel-nfs (expected: async upload overhead + cache misses)
5. JuiceFS metadata (7 ops/s) and small-files (44 ops/s) are extremely poor — likely synchronous S3 writes

## Still To Benchmark
- S3QL (script at /opt/dittofs/bench/infra/scripts/s3ql.sh)
- Rclone S3 (script at /opt/dittofs/bench/infra/scripts/rclone-s3.sh)
- Ganesha (local FS userspace competitor)

## Notes
- Server uses /etc/dfs/config.yaml (not default path)
- Must clean BOTH /.config/dittofs/controlplane.db* AND /root/.config/dittofs/controlplane.db* on restart
- NFSv4.1 mount uses /export path for DittoFS, / for kernel-nfs (fsid=0)
