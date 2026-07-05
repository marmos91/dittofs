# DittoFS Performance Benchmarks

> ⚠️ **LEGACY / STALE — do not cite.** These numbers are from an earlier release and have
> **not** been re-run against current DittoFS. They are kept for historical reference only and
> are intentionally excluded from the published documentation. A fresh benchmark pass is pending;
> until then, treat every figure here as out of date.

Performance comparison of DittoFS with S3 backend against other S3-compatible network filesystems and kernel NFS, on identical Scaleway infrastructure.

## Key Results

**DittoFS S3 dominates every S3-compatible competitor** across all workloads:

![DittoFS vs JuiceFS Performance Ratio](assets/bench-vs-juicefs.png)

| Workload | DittoFS S3 | JuiceFS S3 | Advantage |
|----------|-----------|------------|-----------|
| Sequential Write | 50.7 MB/s | 31.2 MB/s | **1.6x** |
| Sequential Read | 63.9 MB/s | 50.5 MB/s | **1.3x** |
| Random Write | 635 IOPS | 60 IOPS | **10.6x** |
| Random Read | 1,420 IOPS | 1,447 IOPS | ~1x (tied) |
| Metadata | 609 ops/s | 7 ops/s | **87x** |
| Small Files | 1,792 ops/s | 44 ops/s | **41x** |

DittoFS's cache-first architecture means writes never block on S3 — they go to local cache and are uploaded asynchronously in the background. JuiceFS performs synchronous S3 writes on every commit, which destroys metadata and write performance.

## Test Environment

| Parameter | Value |
|-----------|-------|
| Server | Scaleway GP1-XS (4 vCPU, 16 GB RAM, NVMe SSD) |
| Client | Scaleway GP1-XS (separate instance, same AZ) |
| Network | Private LAN (~100 Mbps effective) |
| S3 Backend | Scaleway Object Storage (Paris region) |
| Cache Size | 4 GB on server |
| Duration | 60s per workload |
| File Size | 1 GiB |
| Block Size | 4 KiB |
| Threads | 4 |
| Metadata Files | 1,000 |
| Small File Count | 10,000 |
| NFS Version | NFSv4.1 (primary), NFSv3 (comparison) |

### Systems Tested

| System | Type | S3 Backend | Description |
|--------|------|------------|-------------|
| **DittoFS S3** | Userspace NFS | Scaleway S3 | DittoFS with BadgerDB metadata + S3 payload, 4GB cache |
| **JuiceFS S3** | FUSE + NFS re-export | Scaleway S3 | JuiceFS with Redis metadata + S3 storage |
| **kernel NFS** | Kernel NFS | None (local disk) | Linux knfsd — theoretical upper bound for NFS performance |

## Performance Overview

![Performance Summary Heatmap](assets/bench-summary.png)

Green = DittoFS wins, Red = competitor wins. DittoFS S3 matches or beats kernel NFS (local disk!) on sequential I/O, metadata, and small files. It only trails on random I/O where kernel NFS's direct VFS access has an inherent advantage.

### Performance Profile

![Radar Chart](assets/bench-radar.png)

DittoFS S3 covers the largest area — strong across all dimensions. JuiceFS collapses on metadata, small-files, and random write due to synchronous S3 round-trips.

## Detailed Results

### Sequential Throughput

![Sequential Throughput](assets/bench-throughput.png)

Sequential I/O is **network-limited** on this infrastructure (~50 MB/s write, ~64 MB/s read). DittoFS S3 saturates the link, proving zero overhead on the sequential hot path:

| System | Seq Write | Seq Read |
|--------|-----------|----------|
| **DittoFS S3 (NFSv4.1)** | **50.7 MB/s** | **63.9 MB/s** |
| DittoFS S3 (NFSv3) | 50.8 MB/s | 63.9 MB/s |
| kernel NFS | 49.2 MB/s | 63.9 MB/s |
| JuiceFS S3 | 31.2 MB/s | 50.5 MB/s |

DittoFS S3 actually **beats kernel NFS on sequential write** (50.7 vs 49.2 MB/s = 103%) thanks to the cache-first write path.

### Random I/O

![Random I/O](assets/bench-iops.png)

| System | Rand Write | Rand Read |
|--------|------------|-----------|
| **DittoFS S3 (NFSv4.1)** | **635 IOPS** | **1,420 IOPS** |
| DittoFS S3 (NFSv3) | 634 IOPS | 1,383 IOPS |
| kernel NFS | 1,234 IOPS | 2,241 IOPS |
| JuiceFS S3 | 60 IOPS | 1,447 IOPS |

DittoFS S3 reaches **51% of kernel NFS** on random write and **63% on random read** — expected given the content-addressed cache layer vs kernel NFS's direct VFS access. Against JuiceFS, DittoFS delivers **10.6x more random write IOPS** (635 vs 60).

### Metadata Operations

![Metadata & Small Files](assets/bench-metadata.png)

Metadata measures create + stat + delete cycles on 1,000 files. Small files measures create + read + stat + delete on 10,000 files (1-32 KB each).

| System | Metadata | Small Files |
|--------|----------|-------------|
| **DittoFS S3 (NFSv4.1)** | **609 ops/s** | **1,792 ops/s** |
| DittoFS S3 (NFSv3) | 146 ops/s | 154 ops/s |
| kernel NFS | 290 ops/s | 492 ops/s |
| JuiceFS S3 | 7 ops/s | 44 ops/s |

DittoFS S3 **beats kernel NFS by 2.1x on metadata** (609 vs 290 ops/s) and **3.6x on small files** (1,792 vs 492 ops/s). This is a userspace S3-backed filesystem outperforming the Linux kernel NFS server with local disk.

Against JuiceFS: **87x faster metadata**, **41x faster small files**. JuiceFS's synchronous S3 writes make metadata operations extremely expensive.

### Latency Distribution

![Latency Distribution](assets/bench-latency.png)

DittoFS shows tight, predictable latency across all workloads:

| Workload | DittoFS P50 | DittoFS P99 | kernel NFS P50 | JuiceFS P50 |
|----------|------------|------------|----------------|-------------|
| seq-write | 0.68 ms | 1.51 ms | 0.70 ms | 0.64 ms |
| rand-write | 1.35 ms | 2.81 ms | 0.77 ms | 1.51 ms |
| rand-read | 0.71 ms | 1.01 ms | 0.40 ms | 0.53 ms |
| metadata | 1.00 ms | 4.46 ms | 2.85 ms | 8.55 ms |
| small-files | 2.18 ms | 4.91 ms | 2.40 ms | 8.14 ms |

DittoFS has the **lowest P50 metadata latency** (1.0 ms vs kernel NFS's 2.85 ms) and the **tightest P99 spread** on small files (4.91 ms vs kernel's 27.3 ms and JuiceFS's 949 ms).

## NFSv3 vs NFSv4.1

![NFSv3 vs NFSv4.1](assets/bench-nfs-versions.png)

NFSv4.1 provides dramatic improvements for metadata-heavy workloads on DittoFS:

| Workload | NFSv3 | NFSv4.1 | Improvement |
|----------|-------|---------|-------------|
| metadata | 146 ops/s | 609 ops/s | **4.2x** |
| small-files | 154 ops/s | 1,792 ops/s | **11.6x** |
| rand-read | 1,383 IOPS | 1,420 IOPS | 1.03x |
| rand-write | 634 IOPS | 635 IOPS | ~1x |

NFSv4.1's compound operations (SEQUENCE + PUTFH + OP in a single RPC) eliminate per-operation round trips that dominate NFSv3 metadata performance. **Always use NFSv4.1 with DittoFS.**

## DittoFS vs kernel NFS

DittoFS S3 is a **userspace filesystem writing to cloud object storage** competing against the Linux kernel NFS server with direct local disk access. Despite this fundamental disadvantage:

| Metric | DittoFS S3 | kernel NFS | % of kernel |
|--------|-----------|------------|-------------|
| seq-write | 50.7 MB/s | 49.2 MB/s | **103%** |
| seq-read | 63.9 MB/s | 63.9 MB/s | **100%** |
| rand-write | 635 IOPS | 1,234 IOPS | 51% |
| rand-read | 1,420 IOPS | 2,241 IOPS | 63% |
| metadata | 609 ops/s | 290 ops/s | **210%** |
| small-files | 1,792 ops/s | 492 ops/s | **364%** |

DittoFS beats kernel NFS on **4 of 6 workloads** while providing S3 durability. The only workloads where kernel NFS leads are random I/O, where direct VFS access has an inherent latency advantage over DittoFS's content-addressed cache layer.

## Why DittoFS Is Fast

DittoFS's performance comes from its **cache-first architecture**:

```
NFS WRITE  ──▶  Cache (memory + disk)  ──▶  Return to client immediately
                      │
                      ▼ (async, background)
              Periodic Uploader  ──▶  S3
```

1. **Writes never touch S3** — NFS WRITE goes to local cache, NFS COMMIT flushes to disk. S3 uploads happen asynchronously in the background.
2. **Concurrent NFS dispatch** — Multiple NFS operations execute in parallel per connection.
3. **BadgerDB metadata** — LSM-tree metadata store optimized for write-heavy workloads, outperforming kernel NFS's filesystem-based metadata.
4. **Skip fsync for S3 backends** — The cache is a staging buffer, not the source of truth. Fsync is unnecessary overhead.
5. **Smart block management** — Uploaded blocks are never re-sealed on overwrite, avoiding redundant S3 uploads.

## Optimization History

![Optimization Impact](assets/bench-improvement.png)

Performance improvements from the `feat/cache-rewrite` branch optimization cycle:

| Metric | Round 15 (baseline) | Round 24 (optimized) | Change |
|--------|---------------------|---------------------|--------|
| rand-write | 308 IOPS | 635 IOPS | **+106%** |
| rand-read | 594 IOPS | 1,420 IOPS | **+139%** |
| metadata | 486 ops/s | 609 ops/s | **+25%** |
| small-files | — | 1,792 ops/s | *new workload* |

### Key Optimizations Applied

1. **COMMIT decoupled from S3 upload** — `Flush()` only writes to disk cache, returns immediately
2. **Concurrent NFS dispatch** — goroutine-per-request with bounded semaphore
3. **Skip fsync for S3 backends** — cache is staging buffer, not durable store
4. **GetDirtyBlocks via Flush() return** — eliminates BadgerDB round-trip on commit
5. **Don't re-seal uploaded blocks** — overwrites create new blocks, avoiding redundant uploads
6. **Resettable upload timeout** — uses LastAccess instead of CreatedAt for upload scheduling
7. **Removed runtime.GC()** — eliminated forced garbage collection from periodic uploader

## Running benchmarks

The benchmark suite has three layers, all driven from the `dfsbench`
orchestrator binary plus per-package Go `Benchmark*` tests:

- **In-process micro/macro workloads** — `bench/blockstore`, `bench/snapshots`,
  driven by `dfsbench <area>` or `go test -bench`. No network, no mount.
- **Versioned result documents** — `dfsbench orchestrate` runs a manifest of
  workloads and emits a machine-readable JSON document (host info, per-workload
  ns/op, throughput, latency p50/p95/p99, succeeded/failed op counts, structured
  errors, pprof paths). A compare mode flags ns/op regressions between two runs.
- **Remote infrastructure runs** — `dfsbench remote` drives a benchmark on a
  Scaleway host (provisioned via the `bench/infra` Pulumi stack) over SSH and
  collects the result JSON back.

Build the binary once:

```bash
go build -o dfsbench ./cmd/bench
```

### In-process workloads

```bash
# A single blockstore workload with pprof capture:
./dfsbench blockstore --workload sequential-write --ops 10000
./dfsbench blockstore --workload random-write --ops 5000 --remote=s3 --env-file ./.env

# A manifest of workloads → one versioned result JSON + a summary table:
./dfsbench orchestrate --out result.json --summary

# Compare two result documents (exits non-zero on a regression — CI-gateable):
./dfsbench orchestrate --compare-baseline base.json --compare-candidate new.json
```

The manifest format, full result schema, and the additive-vs-breaking version
contract live in [`bench/orchestrator/README.md`](../bench/orchestrator/README.md).

### Remote runs (Scaleway)

The `bench/infra` Pulumi stack provisions an ephemeral server VM (with a block
volume) and a persistent client VM on a private network. `dfsbench remote` then
drives a run against an already-provisioned host: it reads the server's public
IP (for SSH) and private-network IP (for the mount) from the stack outputs,
scp's a prebuilt `dfsbench` binary to the host, runs `orchestrate` over SSH, and
pulls the result JSON back.

SSH always uses the public IP; the benchmark serves/mounts over the
private-network IP only — the two are kept distinct so a run is never carried
over the public path.

Required setup:

- A Pulumi stack provisioned: `cd bench/infra && pulumi up --stack bench`
  (needs Scaleway credentials in the environment; see `bench/infra/Pulumi.yaml`).
- SSH access to the server's public IP (`--ssh-key` or an agent).
- A `linux/amd64` `dfsbench` build to push.

```bash
# Cross-build the bench binary for the Linux server:
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dfsbench.linux ./cmd/bench

# Resolve the target + print the plan WITHOUT touching the host:
./dfsbench remote --stack bench --dry-run

# Push the binary, run the bench over SSH, fetch the result:
./dfsbench remote --stack bench \
  --binary dfsbench.linux --ssh-key ~/.ssh/id_rsa \
  --out remote-result.json --summary
```

Pass `--private-ip` if the stack does not surface the server's private-network
address (it is assigned by DHCP). Provisioning the VMs themselves stays in
Pulumi; `dfsbench remote` drives an existing host. Live-infra runs cannot be
exercised in CI — the orchestrator's SSH/scp/Pulumi-output logic is unit-tested
with fakes, and `--dry-run` verifies the wiring without credentials.

The remote bench currently runs the in-process `orchestrate` workloads on the
server (no NFS mount), so the resolved private IP is exported to the remote
process as `DITTOFS_BENCH_MOUNT_IP` but not yet consumed for a kernel mount.
That env var is the wiring point for a future client-driven, mount-based runner;
SSH/scp transport already correctly uses the public IP only.

### Regenerating charts

The charts above are generated from saved result JSON:

```bash
python3 -m venv /tmp/bench-charts
/tmp/bench-charts/bin/pip install matplotlib numpy
/tmp/bench-charts/bin/python3 scripts/gen-bench-charts.py
```

Charts are saved to `docs/assets/bench-*.png`.

## Local perf gates

Several in-tree microbenchmark gates protect hot paths under `go test`. They
are skipped under `go test -short`; the heaviest ones are additionally opt-in
via an env var because they allocate several GiB. These run on any machine — no
mount or cloud infra needed.

### Hash + chunker gates

- **BLAKE3 throughput** — `TestBLAKE3AtLeast3xSHA256` (in `pkg/block/`) requires
  BLAKE3 ≥ 3× SHA-256 throughput on amd64 when `D41_STRICT_GATE=1` is set. By
  default, and always on arm64, it relaxes to ≥ 0.5× — Go's `crypto/sha256` uses
  ARMv8 SHA hardware acceleration while `lukechampine.com/blake3` has no NEON
  path on Apple Silicon, so a 3× ratio is not reachable there.
- **FastCDC boundary stability** — `TestChunker_BoundaryStability_70pct` (in
  `pkg/block/chunker/`) requires ≥ 70% of chunk boundaries preserved across
  1–4096 byte shifts of the input stream.

```bash
D41_STRICT_GATE=1 go test -run=TestBLAKE3AtLeast3xSHA256 ./pkg/block/
go test -run=TestChunker_BoundaryStability_70pct ./pkg/block/chunker/
```

### Read-path regression gate

The read-path stack (binary-search lookup over `[]BlockRef`, CAS-keyed cache,
per-share metadata coordinator) is guarded by an in-tree microbench gate in
`pkg/block/engine/`:

- `BenchmarkPerfGate_Phase12RandReadRegression` enforces rand-read IOPS staying
  within 5% of a per-machine microbench floor (the `phase12MicrobenchFloorIOPS`
  constant, conservatively anchored at 50,000 IOPS).
- `TestPerfGate_Phase12_BinarySearchOverhead` caps `findBlocksForRange` average
  cost across a large `[]BlockRef`.

This microbench runs against the in-process memory local store with no remote —
a CPU-floor measurement of the read path, NOT real S3. Real-S3 read throughput
is captured separately by the macro reports above.

```bash
go test -bench=BenchmarkPerfGate_Phase12RandReadRegression -benchtime=10s -run=^$ \
    ./pkg/block/engine/...
go test -run=TestPerfGate_Phase12_BinarySearchOverhead -count=1 -v ./pkg/block/engine/...
```

To re-baseline on a new machine class: capture several runs, take the lowest
ops/sec, multiply by 0.90, and update the floor constant — a deliberate
calibration event that must be reviewed in PR.

### Per-stage write-path profiling (#1555)

The blocks-only write path splits into stages that each own a CPU and allocation
budget. One `Benchmark*` per stage measures it *in isolation* against in-process
memory stores, so a `-cpuprofile` / `-memprofile` run attributes cost to a single
seam instead of a tangled end-to-end flow. All four are plain `testing.B` +
`runtime/pprof` (no harness, no deps): standard
`go test -benchmem -cpuprofile -memprofile` wiring is all that's needed.

| Stage | Bench (package) | Code seam |
| ----- | --------------- | --------- |
| Append log | `BenchmarkAppendWrite_GroupCommit` (`pkg/block/local/fs/`) | `appendwrite.go` + `groupcommit.go` |
| FastCDC chunker | `BenchmarkChunker_Throughput_64MiB` (`pkg/block/chunker/`) | `chunker.go` |
| BLAKE3 hash | `BenchmarkBLAKE3_256MiB` (`pkg/block/`) | content-hash layer |
| Streamer (carve→codec→PutBlock) | `BenchmarkStreamer_CarveCodecPutBlock` (`pkg/block/engine/`) | `carver.go:carveAndCommitBlock` + `blockcodec/codec.go` |

```bash
# Profile any single stage — CPU + heap attributed to that seam only:
go test -bench=BenchmarkStreamer_CarveCodecPutBlock -benchmem -run=^$ \
    -cpuprofile=cpu.out -memprofile=mem.out ./pkg/block/engine/
go tool pprof -top cpu.out      # where the CPU went
go tool pprof -top -alloc_space mem.out   # where the bytes were allocated
```

Two memory invariants are pinned as hard tests (fail the build, not just report):

- `TestChunker_ConstantMemory` (`pkg/block/chunker/`) — the FastCDC scan is
  allocation-free regardless of input size; `Next` must report offsets into the
  caller's slice, never copy chunk bytes.
- `TestStreamer_AllocationTracksBlockCount` (`pkg/block/engine/`) — the streamer's
  allocation grows linearly with block count, not super-linearly with file size;
  it must carve one block at a time, never buffer the whole file.

GC + compaction is intentionally **not** benched here: #1487 reshaped that path
and it is latency/sweep-bound rather than a CPU/alloc hot spot on the write path —
a dedicated bench is deferred until there is a measured reason to add one.

#### Baseline reading (Apple M1 Max, in-memory, `-benchtime=5x`)

| Stage | ns/op | Throughput | B/op | allocs/op |
| ----- | ----: | ---------: | ---: | --------: |
| Append log (coalesced 64 B) | 4.6 ms | — | 353 | 6 |
| FastCDC chunker (64 MiB) | 42.2 ms | 1,592 MB/s | **0** | **0** |
| BLAKE3 hash (256 MiB) | 53.8 ms | 4,994 MB/s | 3.7 MB | 35,713 |
| Streamer (64 MiB) | 19.6 ms | 3,426 MB/s | **346 MB** | 753 |

Normalizing to cost-per-MiB pushed through the write path:

- **CPU is dominated by the FastCDC chunker** — 0.63 ms/MiB, ~3× the hash
  (0.20 ms/MiB) and ~2× the streamer (0.29 ms/MiB of codec framing + remote body
  copy; hashing is the separate BLAKE3 stage above and is precomputed in the
  streamer fixture, matching production carve, which reuses each chunk's
  write-time hash). The boundary scan, not framing or upload, is the write-path
  CPU floor.
- **Allocations are dominated by the streamer** — ~346 MB allocated per 64 MiB
  carved (~5× write amplification), from `bytes.Buffer` doubling growth while
  assembling each block plus the remote body copy. The chunker is allocation-free;
  the streamer is the only stage with a real allocation lever, and the cheap fix is
  pre-sizing the carve buffer to the target block size — no format or sizing change.

Gate call for the deferred perf issues this profiling was meant to decide:

- **#1491 (decoupled log-blob / block sizing) — not justified by this data.** The
  streamer's allocation cost is transient buffer growth, not sizing granularity;
  a one-line buffer pre-size captures the win without decoupling the two sizes.
- **#1488 (chunk-range refetch granularity) — read-path, not a write-path
  bottleneck here.** The warm read path already avoids the WAN round-trip
  (`BenchmarkReadThroughCache_ColdVsWarm`), so there is no measured pressure to
  build finer read-miss granularity yet.

Re-run all four on a new machine class before trusting the ranking — the absolute
numbers are machine-specific; the *ordering* (chunker = CPU, streamer = allocs) is
the durable finding.

### A/B comparing commits

For ad-hoc before/after work, run any package's `Benchmark*` against two commits
and diff with `benchstat`:

```bash
go test -bench=. -count=10 -run=^$ ./bench/blockstore/ > before.txt
# ... check out the other commit ...
go test -bench=. -count=10 -run=^$ ./bench/blockstore/ > after.txt
benchstat before.txt after.txt
```

## Snapshot scale limits

Snapshot `create` does a metadata `Backup` (a streamed dump plus an in-RAM
`HashSet` of every referenced block hash), writes a hash manifest, drains
uploads, then verifies durability by HEAD-probing every manifest hash at
concurrency 16. `restore` reads the manifest back, resets, restores the dump,
and re-verifies. The workloads in `bench/snapshots/` isolate the three cost
centers (backup, manifest, verify) so a single benchmark can sweep file counts
without standing up adapters / the control-plane DB / real S3.

```bash
# CI-safe sweep (1e4 / 1e5 files; 1e6 cases skipped under -short):
go test -bench=. -benchmem -short -run=^$ ./bench/snapshots/

# Full sweep including 1e6-file scales (heavy — minutes, multi-GB allocs):
go test -bench=. -benchmem -benchtime=1x -run=^$ -timeout=900s ./bench/snapshots/

# One ad-hoc seed→backup→manifest→verify pass with per-stage wall time:
./dfsbench snapshots --files 1000000 --blocks-per-file 8
```

### Indicative numbers (Apple M1 Max, memory engine, in-memory remote)

All-unique blocks (`--dedup 1`, the worst case for HashSet + manifest RAM),
`benchtime=1x`. `dump_bytes` is streamed to a discard writer — it is the
serialized dump size, not a resident buffer.

| Scale (files × blocks) | unique hashes | backup ns/op | dump_bytes | manifest_bytes | verify ns/op (probes) |
| ---------------------- | ------------: | -----------: | ---------: | -------------: | --------------------: |
| 1e5 × 1                |       100,000 |        1.15 s |    35.0 MB |        6.5 MB |     0.14 s (100,000) |
| 1e5 × 8                |       800,000 |        1.45 s |    67.2 MB |       52.0 MB |     1.39 s (800,000) |
| 1e6 × 1                |     1,000,000 |        5.92 s |   350.0 MB |       65.0 MB |     1.95 s (1,000,000) |
| 1e6 × 8                |     8,000,000 |       18.25 s |   672.0 MB |      520.0 MB |    25.27 s (8,000,000) |

### Established limits & budget

- **The badger dump is streamed.** The badger engine (KV-by-KV) and the manifest
  writer emit to an `io.Writer` without buffering the whole dump. The dominant
  create-path resident allocation is then the returned `HashSet`: one 32-byte
  `ContentHash` per **unique** block, ~26 B/entry in the Go map. **Budget ~25 MB
  of HashSet RAM per 1 M unique blocks**; 8 M unique blocks ≈ 200 MB.
- **Manifest on disk is 65 bytes/hash** (64 hex + LF): 65 MB per 1 M hashes,
  520 MB at 8 M. Written streamed; read back into a resident HashSet on restore.
- **Verify is N HEAD round-trips at concurrency 16**, holding nothing across
  probes. The in-memory-remote times above are a floor with zero network
  latency. For an S3 budget, multiply the probe count by the real per-HEAD RTT ÷
  16: e.g. 8 M probes at 20 ms/HEAD ≈ 167 minutes of verify, plus 8 M HEAD
  charges. Large shares should size their verify window from the manifest hash
  count, or create with `--no-verify` and accept `remote_durable=false`.
- **The memory metadata engine is not suitable for TB/M-file shares.** It
  gob-encodes its entire snapshot into one buffer during Backup. **Use the
  badger engine for large shares; it streams the dump KV-by-KV.** Badger restore
  also streams: entries apply via a bounded `WriteBatch`, the integrity CRC is
  verified last, and any failure triggers `DropAll` to leave the store
  empty/retryable.

## Raw data

Each result JSON contains per-workload metrics: throughput/IOPS, latency
percentiles (p50/p95/p99), total + succeeded + failed operation counts, and
structured per-op errors. `dfsbench orchestrate --summary` prints the same data
as a table.
