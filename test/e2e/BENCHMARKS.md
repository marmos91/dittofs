# DittoFS perf benchmarks & gates

This page is the single place contributors look for local perf gates wired
into the Go test suite. Full end-to-end performance reports (DittoFS vs
JuiceFS vs kernel NFS on real infrastructure) live in `docs/BENCHMARKS.md`.

## Local-store perf gates (D-40 / D-41 / D-42)

The hybrid append-log local store ships three in-tree microbenchmark
gates. All three are skipped under `go test -short`. D-40 is additionally
gated on the `D40_GATE` env var because it allocates several GiB of disk
and runs for minutes.

Run locally from the repo root:

```bash
# D-40: AppendWrite median ns/op must be <= 1.15 * tryDirectDiskWrite
# median ns/op on a 1 GiB sequential write (1 MiB chunks). Runs each
# benchmark 5 times with auto-tuned b.N and compares medians.
#
# Opt-in via D40_GATE=1 because it allocates ~5 GiB of tempdir space and
# takes minutes on typical dev hardware.
D40_GATE=1 go test -run=TestAppendWriteWithin15pct_D40 -timeout=15m \
    ./pkg/blockstore/local/fs/

# D-41: BLAKE3 >= 3x SHA-256 throughput on 256 MiB (amd64 gate).
# On arm64 the gate relaxes to >= 1x because Go's crypto/sha256 uses
# ARMv8 SHA hw acceleration while BLAKE3 still falls back to portable
# Go on most Apple Silicon chips. See hash_bench_test.go for the full
# rationale.
go test -run=TestBLAKE3AtLeast3xSHA256 ./pkg/blockstore/

# D-42: FastCDC boundary stability >= 70% preserved across 1-4096 byte
# shifts of the input stream.
go test -run=TestChunker_BoundaryStability_70pct ./pkg/blockstore/chunker/
```

### Interpreting D-40 output

On success the gate logs two lines, e.g.:

```
D-40 medians over 5 runs: append=1042857000 ns/op legacy=1012345000 ns/op ratio=1.03 (limit 1.15)
D-40 gate met: ratio=1.03 <= 1.15
```

`ratio = median(AppendWrite ns/op) / median(legacy ns/op)`. Anything
below 1.15 is a pass. Because each run takes minutes and b.N is
auto-tuned, the **absolute** numbers will vary across machines — trend
the ratio, not the ns/op.

On failure the gate fails the test with both medians and the ratio so
the regression is immediately attributable.

### Why D-40 is not a CI gate yet

The original design speced a dedicated CI perf lane with stable
hardware and baseline capture, but standing it up required more infra
work than budget allowed — the fallback is "in-tree gate + local-run
instructions" (this document).

**The CI perf lane is a prerequisite for fail-closed enforcement.**
Once the lane exists it can enable this gate (and D-41 / D-42) by
setting `D40_GATE=1` and dropping `-short` in that job.

D-40 was originally speced as a 5% gate; it was later loosened to 15%
trend mode with a 5-run median after demonstrating that single-run
benches without warmup flap on 5% tolerances on developer laptops.

## Running the paired benchmarks directly

For ad-hoc profiling (e.g., flame graphs of AppendWrite vs the legacy
path) you can run either benchmark on its own:

```bash
go test -run=^$ -bench=BenchmarkAppendWrite_Sequential1GiB \
    -benchtime=3x -count=3 ./pkg/blockstore/local/fs/

go test -run=^$ -bench=BenchmarkTryDirectDiskWrite_Sequential1GiB \
    -benchtime=3x -count=3 ./pkg/blockstore/local/fs/
```

`-benchtime=3x` forces exactly 3 iterations (3 GiB written) so the run
time is predictable. Use `-cpuprofile=cpu.out` / `-memprofile=mem.out`
to collect profiles.

## CAS read/write perf gates (D-20)

The streaming BLAKE3 verifier protects the CAS read path (INV-06) and
the CAS write path (BSCAS-01/03/06) ships with two gates:

- **Verifier gate (D-20):** rand-read-with-verifier must be within 5%
  IOPS of rand-read-without-verifier on a real S3 backend.
- **Write-path gate (≤6% global budget per STATE.md):** rand-write CAS
  must be within 6% of the recorded rand-write baseline.

Both gates ship as in-tree microbenchmarks in
`pkg/blockstore/engine/perf_bench_test.go`. The test
`TestPerfGate_VerifierWithinBudget` programmatically runs both
rand-read benches and compares the regression. The hard 5% fail-closed
enforcement is opt-in via `D20_STRICT_GATE=1` because the in-process
in-memory remote (used to keep the bench network-free) makes the
unverified baseline a memcpy and the verifier appears as pure BLAKE3
overhead — that comparison is instructive but not the production gate.

### Reproduction commands

```bash
# Inline gate test (informational by default; pair with D20_STRICT_GATE=1
# on the dedicated CI perf lane against a real S3 backend).
go test -run TestPerfGate_VerifierWithinBudget ./pkg/blockstore/engine/ \
    -count=1 -v

# Full bench output (run twice for variance signal).
go test -bench='BenchmarkRandReadVerified|BenchmarkRandReadUnverified|BenchmarkRandWriteCAS' \
    -benchtime=10s ./pkg/blockstore/engine/ -run='^$' -benchmem -count=2 \
    | tee /tmp/cas-bench.txt
```

### Indicative local numbers (Apple Silicon)

These numbers are **indicative** — they were captured against the
in-memory remote (no network, no AWS SDK), so they represent the CPU
floor of each path on this hardware. Hard CI gating against real S3
or Localstack remains a follow-up that requires the dedicated bench
lane (D-43 prereq).

- **Date:** 2026-04-25
- **Git SHA:** `a3f05722` (worktree base `4219a61d`)
- **Hardware:** Apple M1 Max, 10 cores (Darwin arm64)
- **Benchtime:** 5s, count=2 per benchmark

| Benchmark                    | ns/op     | MB/s   | ops/s |  B/op   | allocs/op |
| ---------------------------- | --------- | ------ | ----: | ------: | --------: |
| BenchmarkRandReadVerified    | 1,101,469 |  3,808 |   908 | 4269410 |       569 |
| BenchmarkRandReadVerified    | 1,101,121 |  3,809 |   908 | 4269410 |       569 |
| BenchmarkRandReadUnverified  |   121,907 | 34,406 | 8,203 | 4194304 |         1 |
| BenchmarkRandReadUnverified  |   122,319 | 34,290 | 8,175 | 4194304 |         1 |
| BenchmarkRandWriteCAS        | 3,618,774 |  1,159 |   276 | 8473447 |       584 |
| BenchmarkRandWriteCAS        | 3,642,965 |  1,151 |   274 | 8473561 |       584 |

Computed regressions (lower is better):

- **Verifier overhead (in-memory baseline):** `1 - (908 / 8189) ≈ 88.9%`.
  Against an in-memory remote whose unverified baseline is essentially a
  memcpy, this measures the cost of BLAKE3-256 over a 4 MiB buffer
  (~3.8 GB/s on the M1 Max portable-Go BLAKE3 path) plus the
  verifyingReader allocation overhead. It is **not** the real-S3 5%
  number — once network/AWS SDK cost dominates the unverified path, the
  marginal verifier cost shrinks accordingly.
- **CAS write throughput:** ~275 ops/s (≈ 1.15 GB/s steady state). The
  rand-write baseline is recorded against on-disk + S3 paths in
  `docs/BENCHMARKS.md`; the in-memory CAS write number here is a
  CPU-floor reference for the upload-path implementation cost (BLAKE3
  hash + memcpy into the in-memory remote + metadata-txn). Real ≤6%
  budget enforcement needs the bench lane against a real S3 endpoint.

### How to enforce on the CI perf lane (follow-up)

When the CI perf lane lands:

1. Run the rand-read benches against a real S3 backend (Localstack
   reused from `test/e2e/`, or a dedicated bucket on the bench rig).
2. Set `D20_STRICT_GATE=1` so `TestPerfGate_VerifierWithinBudget` fails
   the build if the regression exceeds 5%.
3. Compare `BenchmarkRandWriteCAS` against the rand-write baseline
   captured in `docs/BENCHMARKS.md` and fail the build at 6%.
4. Record each run's date + git SHA + hardware + numbers in this
   document so trend hunting works across releases.

Until then the inline gate test passes by design and the benchmarks
are run on demand for trend visibility.

## Read-path perf gate (D-43)

The read-path stack carries new risk surface: binary-search lookup
over `[]BlockRef`, CAS-keyed Cache, and the per-share metadata
coordinator. D-43 is the hard regression gate that blocks merge until
rand-read latency stays within the per-machine microbench floor.

**Gate budget:** ≤5% rand-read regression vs the per-machine in-tree
microbench floor. Tighter than the global ≤6% per STATE.md so
downstream changes have headroom before the global 6% budget is
exhausted.

### Microbench vs real-S3 disclaimer

The in-tree microbench (`BenchmarkPerfGate_Phase12RandReadRegression`)
runs against the in-process memory local store + nil remote, NOT
real S3. The ≥1,350 IOPS rand-read figure in the milestone notes is
the **bench/infra real-S3 lane** number — Pulumi-deployed Scaleway
nodes against an `s3.fr-par.scw.cloud` bucket. The two are NOT
directly comparable: the microbench is a CPU-floor measurement of
the engine's read path (binary search + Cache.OnRead + buffer copy)
while the real-S3 lane bottleneck is network + AWS SDK + dedup-cache
hit-rate.

The microbench gate uses a per-machine-calibrated floor (a numeric
constant in `perf_bench_phase12_test.go::phase12MicrobenchFloorIOPS`)
recorded by the first run on a new machine class. Real-S3 perf is
verified separately at the milestone gate VER-02.

| Gate                                                                 | Tolerance                              | Test                                          |
| -------------------------------------------------------------------- | -------------------------------------- | --------------------------------------------- |
| rand-read in-tree microbench >= `phase12MicrobenchFloorIOPS` IOPS    | <= 5% regression vs per-machine floor  | `BenchmarkPerfGate_Phase12RandReadRegression` |
| findBlocksForRange average <1 µs/call across 16K BlockRefs           | hard ceiling                           | `TestPerfGate_Phase12_BinarySearchOverhead`   |

### Reproduction commands

Local runs use the `bench-phase12` Makefile target (10 s benchtime,
deterministic `-run=^$`):

```bash
make bench-phase12

# Or directly:
go test -bench BenchmarkPerfGate_Phase12 -benchtime=10s -run=^$ \
    ./pkg/blockstore/engine/...

# Supporting gates (run as normal tests, fast):
go test -run 'TestPerfGate_Phase12' -count=1 -v ./pkg/blockstore/engine/...
```

### Indicative microbench numbers (Apple Silicon)

Use these as a sanity check, not as the gate floor — the gate
compares against the conservative `phase12MicrobenchFloorIOPS`
constant (50,000 IOPS) which is anchored well below the M1 Max
measurement to avoid CI flakes.

- **Date:** 2026-04-27
- **Hardware:** Apple M1 Max, 10 cores (Darwin arm64)
- **Benchtime:** 2s (sanity), 10s (gate)
- **Configuration:** 64 MiB payload, 4 MiB BlockRefs, 4 KiB reads, in-memory local store

| Benchmark                                          | ops/sec   | ns/op | MB/s   | B/op | allocs/op |
| -------------------------------------------------- | --------: | ----: | -----: | ---: | --------: |
| BenchmarkRandRead_Phase12                          |   570,000 |  1,752 | 2,338  | 2806 |        15 |
| BenchmarkPerfGate_Phase12RandReadRegression (gate) |   348,000 |  2,867 | 1,429  | 2823 |        16 |

The gate's per-iteration variance comes from the b.N auto-tuner +
prefetch worker pool warm-up — every recorded run on this machine
sat well above the 47,500 IOPS floor (50K × 0.95).

### Re-baselining on a new machine

If the gate fails on a new CI runner / dev machine because the floor
constant is not appropriate for that machine class:

1. Run `make bench-phase12 -count=5` to capture five runs.
2. Take the lowest ops/sec figure across all runs.
3. Multiply by 0.90 (10% margin below the worst observed).
4. Update `phase12MicrobenchFloorIOPS` in `perf_bench_phase12_test.go`.
5. Append a row to the table above with date, hardware, and the new
   floor.

Re-baselining is a deliberate calibration event, not a fix — it MUST
be reviewed in PR.

## BenchmarkRandReadVerified — warm-cache regression gate

The cache's platform-aware mmap fast-path was removed from
`pkg/blockstore/engine/cache.go`. The warm-cache path never touched
mmap — the LRU stored `[]byte` copies regardless — so the regression
is bounded by ≤1.02 vs the pre-removal baseline. Verified empirically
rather than assumed.

### Reproduction

```bash
# Baseline (in a worktree at the pre-deletion commit):
git worktree add /tmp/dittofs-baseline f8e2532d
cd /tmp/dittofs-baseline && go test -bench=BenchmarkRandReadVerified \
    -benchtime=10s -count=3 -run='^$' ./pkg/blockstore/engine/... \
    > /tmp/randread-pre.txt
git worktree remove /tmp/dittofs-baseline --force

# Current tree:
go test -bench=BenchmarkRandReadVerified -benchtime=10s -count=3 \
    -run='^$' ./pkg/blockstore/engine/... > /tmp/randread-post.txt

benchstat /tmp/randread-pre.txt /tmp/randread-post.txt
```

### Result (Apple M1 Max)

- **Pre commit:**  `f8e2532d`
- **Post commit:** `436a81ec`
- **Benchtime:**   `10s`, **count:** `3` per bench
- **Hardware:**    Apple M1 Max, 10 cores (Darwin arm64)

| Benchmark (single chunk size: 4 MiB) | Pre median ns/op | Post median ns/op | Ratio post/pre | Gate (≤1.02) |
| ------------------------------------ | ---------------: | ----------------: | -------------: | :----------: |
| BenchmarkRandReadVerified            |        1,492,970 |         1,328,307 |          0.890 |     PASS     |

| Metric    | Pre median | Post median |
| --------- | ---------: | ----------: |
| ops/s     |      669.8 |       752.8 |
| MB/s      |   2,809.37 |    3,157.63 |
| B/op      |  4,269,411 |   4,269,410 |
| allocs/op |        569 |         569 |

`benchstat` summary:

```
                    │ /tmp/randread-pre.txt  │      /tmp/randread-post.txt     │
                    │         sec/op         │    sec/op     vs base           │
RandReadVerified-10             1.493m ± ∞ ¹   1.328m ± ∞ ¹  ~ (p=0.700 n=3) ²
                    │          B/s           │      B/s       vs base           │
RandReadVerified-10            2.616Gi ± ∞ ¹   2.941Gi ± ∞ ¹  ~ (p=0.700 n=3) ²
                    │         ops/s          │    ops/s     vs base           │
RandReadVerified-10              669.8 ± ∞ ¹   752.8 ± ∞ ¹  ~ (p=0.700 n=3) ²
```

Post is slightly faster than pre — the `loadByHash` closure no longer
dispatches through the per-OS mmap thunk; `B/op` and `allocs/op` are
bit-identical because the mmap path already copied bytes into the
LRU slot, so the alloc count was unchanged. The ≤1.02 gate is met
with a wide margin.

### What was deleted

The per-OS cache mmap files were removed:
`pkg/blockstore/engine/cache_mmap_unix.go`,
`pkg/blockstore/engine/cache_mmap_windows.go`,
`pkg/blockstore/engine/cache_mmap_test.go`, and
`pkg/blockstore/engine/perf_bench_unix_test.go` (which held a gate
that measured `mmap` vs `os.ReadFile`, both meaningless without the
mmap loader).

### Cold-cache benchmark — intentionally omitted

This gate suite intentionally does not include a cold-cache benchmark.
Production workloads are warm; `BenchmarkRandReadVerified` is the
canonical warm-cache regression anchor. A dedicated
`BenchmarkRandReadVerified_ColdCache` (clearing LRU before each read,
gate ≤1.10) will be added only if cold-read complaints surface in
production.

## Snapshot scale limits

Snapshot `create` does a metadata `Backup` (a streamed dump plus an in-RAM
`HashSet` of every referenced block hash), writes a hash manifest, drains
uploads, then verifies durability by HEAD-probing every manifest hash at
concurrency 16. `restore` reads the manifest back, resets, restores the
dump, and re-verifies. None of these had a benchmark; the numbers below
establish the memory ceiling and the verify budget before any large-share
deployment claim.

Workloads live in `bench/snapshots/` and run via the `dfsbench snapshots`
CLI or the package `Benchmark*` tests. They isolate the three cost centers
(backup, manifest, verify) from the Runtime orchestration so a single
benchmark can sweep file counts without standing up adapters / the
control-plane DB / real S3.

### Reproduction commands

```bash
# CI-safe sweep (1e4 / 1e5 files; 1e6 cases skipped under -short):
go test -bench=. -benchmem -short -run=^$ ./bench/snapshots/

# Full sweep including 1e6-file scales (heavy — minutes, multi-GB allocs):
go test -bench=. -benchmem -benchtime=1x -run=^$ -timeout=900s ./bench/snapshots/

# One ad-hoc seed→backup→manifest→verify pass with per-stage wall time:
go build -o dfsbench ./cmd/bench
./dfsbench snapshots --files 1000000 --blocks-per-file 8
```

### Indicative numbers (Apple M1 Max, memory engine, in-memory remote)

All-unique blocks (`--dedup 1`, the worst case for HashSet + manifest RAM).
`benchtime=1x`. `dump_bytes` is streamed to a discard writer — it is the
serialized dump size, not a resident buffer.

| Scale (files × blocks) | unique hashes | backup ns/op | dump_bytes | manifest_bytes | verify ns/op (probes) |
| ---------------------- | ------------: | -----------: | ---------: | -------------: | --------------------: |
| 1e5 × 1                |       100,000 |        1.15 s |    35.0 MB |        6.5 MB |     0.14 s (100,000) |
| 1e5 × 8                |       800,000 |        1.45 s |    67.2 MB |       52.0 MB |     1.39 s (800,000) |
| 1e6 × 1                |     1,000,000 |        5.92 s |   350.0 MB |       65.0 MB |     1.95 s (1,000,000) |
| 1e6 × 8                |     8,000,000 |       18.25 s |   672.0 MB |      520.0 MB |    25.27 s (8,000,000) |

`write-manifest` and `read-manifest` (restore pre-verify) at 1e6 × 8:
6.20 s / 16.18 s; the manifest parses back into a resident HashSet
(~4.2 GB B/op at 8 M hashes, dominated by the per-line hex decode + map
insert).

### Established limits & budget

- **The badger dump is streamed.** The badger engine (KV-by-KV) and the
  manifest writer emit to an `io.Writer` without buffering the whole dump;
  on the badger path `dump_bytes` never lands in a single allocation. The
  dominant create-path resident allocation is then the returned `HashSet`:
  one 32-byte `ContentHash` per **unique** block, ~26 B/entry in the Go
  map. **Budget ~25 MB of HashSet RAM per 1 M unique blocks**; 8 M unique
  blocks ≈ 200 MB. (The memory engine does NOT stream — see the last
  bullet; the indicative table above uses the memory engine, so its
  create-path `B/op` reflects that buffer, not the streaming ceiling.)
- **Manifest on disk is 65 bytes/hash** (64 hex + LF): 65 MB per 1 M
  hashes, 520 MB at 8 M. Written streamed; read back into a resident
  HashSet on restore (size as above).
- **Verify is N HEAD round-trips at concurrency 16**, holding nothing
  across probes. The in-memory-remote times above are a **floor with zero
  network latency**. For an S3 budget, multiply the probe count by the real
  per-HEAD RTT ÷ 16: e.g. 8 M probes at 20 ms/HEAD ≈ 8e6 × 0.02 / 16 ≈
  **167 minutes** of verify, plus 8 M HEAD-request charges. Large shares
  should size their verify window (and S3 request cost) from the manifest
  hash count, or create with `--no-verify` and accept `remote_durable=false`.
- **The memory metadata engine is not suitable for TB/M-file shares.** It
  gob-encodes its entire snapshot into one buffer during Backup (expected
  for an in-RAM backend) — the create-path `B/op` for the memory engine
  reflects that buffer, not the dump stream. **Use the badger engine for
  large shares; it streams the dump KV-by-KV.** Badger restore also
  streams: KV entries apply via a bounded `WriteBatch`, the integrity CRC
  is verified last, and any failure triggers `DropAll` to leave the store
  empty/retryable — so neither badger create nor badger restore buffers
  the whole dump.

## End-to-end performance reports

For NFSv3/NFSv4.1 + SMB end-to-end numbers against kernel NFS and
JuiceFS on Scaleway infrastructure, see `docs/BENCHMARKS.md`.
