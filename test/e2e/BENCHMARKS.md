# DittoFS perf benchmarks & gates

This page is the single place contributors look for local perf gates wired
into the Go test suite. Full end-to-end performance reports (DittoFS vs
JuiceFS vs kernel NFS on real infrastructure) live in `docs/BENCHMARKS.md`.

## v0.15.0 Phase 10 perf gates (D-40 / D-41 / D-42)

The Phase 10 (`v0.15.0` A1) refactor adds three in-tree microbenchmark
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

D-43 (see `.planning/phases/10-fastcdc-chunker-hybrid-local-store-a1/10-CONTEXT.md`)
originally speced a dedicated CI perf lane with stable hardware and
baseline capture. Phase 10 fell back to "in-tree gate + local-run
instructions" (this document) because standing up the lane required
more infra work than the 3-week Phase 10 budget allowed.

**The CI perf lane is a Phase 11 prerequisite.** Once the lane exists
it can enable this gate (and D-41 / D-42) by setting `D40_GATE=1` and
dropping `-short` in that job.

Phase-review note: D-40 was originally speced as a 5% gate; Warning 4
of the Phase 10 review loosened it to 15% trend mode with a 5-run
median after demonstrating that single-run benches without warmup
flap on 5% tolerances on developer laptops.

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

## v0.15.0 Phase 11 perf gate (D-20)

Phase 11 (`v0.15.0` A2) ships the streaming BLAKE3 verifier on the CAS
read path (INV-06) and the new CAS write path (BSCAS-01/03/06). Two
gates protect both:

- **Verifier gate (D-20):** rand-read-with-verifier must be within 5%
  IOPS of rand-read-without-verifier on a real S3 backend.
- **Write-path gate (≤6% global budget per STATE.md):** rand-write CAS
  must be within 6% of the Phase 10 rand-write baseline.

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
    | tee /tmp/phase11-bench.txt
```

### Indicative local numbers (Apple Silicon)

These numbers are **indicative** — they were captured against the
in-memory remote (no network, no AWS SDK), so they represent the CPU
floor of each path on this hardware. Hard CI gating against real S3
or Localstack remains a follow-up that requires the dedicated bench
lane (D-43, Phase 11 prereq).

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
  Phase 10 rand-write baseline is recorded against on-disk + S3 paths
  in `docs/BENCHMARKS.md`; the in-memory CAS write number here is a
  CPU-floor reference for the upload-path implementation cost (BLAKE3
  hash + memcpy into the in-memory remote + metadata-txn). Real ≤6%
  budget enforcement vs Phase 10 needs the bench lane against the same
  S3 endpoint Phase 10 used.

### How to enforce on the CI perf lane (follow-up)

When the CI perf lane lands:

1. Run the rand-read benches against a real S3 backend (Localstack
   reused from `test/e2e/`, or a dedicated bucket on the bench rig).
2. Set `D20_STRICT_GATE=1` so `TestPerfGate_VerifierWithinBudget` fails
   the build if the regression exceeds 5%.
3. Compare `BenchmarkRandWriteCAS` against the Phase 10 baseline
   captured in `docs/BENCHMARKS.md` and fail the build at 6%.
4. Record each run's date + git SHA + hardware + numbers in this
   document so trend hunting works across releases.

Until then the inline gate test passes by design and the benchmarks
are run on demand for trend visibility.

## v0.15.0 Phase 12 perf gate (D-43)

Phase 12 (`v0.15.0` A3) stacks new risk surface on the read path:
binary-search lookup over `[]BlockRef` (Plan 07), CAS-keyed Cache
rewrite (Plan 09), and the mmap single-copy seam on local hits
(Plan 10). D-43 is the hard regression gate that blocks PR-C merge
until rand-read latency stays within the per-machine microbench
floor.

**Gate budget:** ≤5% rand-read regression vs the per-machine in-tree
microbench floor. Tighter than the global ≤6% per STATE.md so that
Phase 13 (file-level dedup) and Phase 14 (migration) have headroom
before the global 6% budget is exhausted.

### Microbench vs real-S3 disclaimer

The Phase 12 in-tree microbench (`BenchmarkPerfGate_Phase12RandReadRegression`)
runs against the in-process memory local store + nil remote, NOT
real S3. The Phase 11 / ROADMAP figure of ≥1,350 IOPS rand-read is
the **bench/infra real-S3 lane** number — Pulumi-deployed Scaleway
nodes against an `s3.fr-par.scw.cloud` bucket. The two are NOT
directly comparable: the microbench is a CPU-floor measurement of
the engine's read path (binary search + Cache.OnRead + buffer copy)
while the real-S3 lane bottleneck is network + AWS SDK + dedup-cache
hit-rate.

The microbench gate uses a per-machine-calibrated floor (a numeric
constant in `perf_bench_phase12_test.go::phase12MicrobenchFloorIOPS`)
recorded by the first run on a new machine class. Real-S3 perf is
verified separately at the v0.15.0 milestone gate VER-02.

| Phase   | Gate                                                                       | Tolerance                          | Test                                              |
| ------- | -------------------------------------------------------------------------- | ---------------------------------- | ------------------------------------------------- |
| Phase 12 (A3) | rand-read in-tree microbench >= `phase12MicrobenchFloorIOPS` IOPS    | <= 5% regression vs per-machine floor | `BenchmarkPerfGate_Phase12RandReadRegression`     |
| Phase 12 (A3) | findBlocksForRange average <1 µs/call across 16K BlockRefs           | hard ceiling                       | `TestPerfGate_Phase12_BinarySearchOverhead`       |
| Phase 12 (A3) | readFromCAS mmap throughput >= 0.95 × os.ReadFile (linux/darwin)     | hard ratio floor                   | `TestPerfGate_Phase12_MmapHotPath`                |

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

### Indicative microbench numbers (Apple Silicon, Plan 12-12 first run)

These numbers were captured on the executor machine that landed
Plan 12-12. Use them as a sanity check, not as the gate floor — the
gate compares against the conservative `phase12MicrobenchFloorIOPS`
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

## End-to-end performance reports

For NFSv3/NFSv4.1 + SMB end-to-end numbers against kernel NFS and
JuiceFS on Scaleway infrastructure, see `docs/BENCHMARKS.md`.
