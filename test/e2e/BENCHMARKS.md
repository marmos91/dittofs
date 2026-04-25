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

## End-to-end performance reports

For NFSv3/NFSv4.1 + SMB end-to-end numbers against kernel NFS and
JuiceFS on Scaleway infrastructure, see `docs/BENCHMARKS.md`.
