// Package fs — Phase 10 plan 11: D-40 perf gate benchmarks.
//
// Paired benchmarks for AppendWrite (new append-log path) vs the legacy
// WriteAt / tryDirectDiskWrite path, plus a median-of-5 test gate that
// enforces AppendWrite median ns/op <= 1.15 * legacy median ns/op on a
// 1 GiB sequential-write workload.
//
// See 10-11-PLAN.md and 10-CONTEXT.md (D-40, D-43) for the full rationale:
//   - D-40 originally speced a 5% gate; Warning 4 of the phase review
//     loosened to 15% trend-mode with 5-run median after showing that
//     single-run benches without warmup flap on 5% tolerances.
//   - D-43 chose "in-tree gate + manual run" over a dedicated CI perf
//     lane (standing up the lane is a Phase 11 prerequisite).
//
// The gate is opt-in via the D40_GATE env var and additionally skipped
// under -short so normal CI lanes stay green. Run locally with:
//
//	D40_GATE=1 go test -run=TestAppendWriteWithin15pct_D40 -timeout=15m \
//	    ./pkg/blockstore/local/fs/
package fs

import (
	"bytes"
	"context"
	"os"
	"sort"
	"testing"

	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// benchPayloadLen is the chunk size written on each AppendWrite / WriteAt
// call. 1 MiB matches the NFS wsize ceiling used by the kernel client when
// the server advertises 1 MiB max, so this exercises the ordinary large-
// sequential hot path.
const benchPayloadLen = 1 * 1024 * 1024 // 1 MiB

// benchTotalBytes is the total amount of data written per benchmark
// iteration. 1 GiB matches the D-40 spec (see 10-CONTEXT.md).
const benchTotalBytes = 1 * 1024 * 1024 * 1024 // 1 GiB

// benchStoreAppend constructs an append-log-enabled FSStore sized so the
// benchmark never triggers a log rollover or a rollup flush.
func benchStoreAppend(b *testing.B) *FSStore {
	b.Helper()
	dir := b.TempDir()
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc, err := NewWithOptions(dir, 1<<40, 1<<40, nopFBS{}, FSStoreOptions{
		UseAppendLog:  true,
		MaxLogBytes:   1 << 34, // 16 GiB — effectively unbounded for this bench
		RollupWorkers: 2,
		// StabilizationMS is very long so rollup stays out of the way for the
		// duration of the benchmark. Rollup competition would pollute the
		// measured AppendWrite hot path.
		StabilizationMS: 10000,
		RollupStore:     rs,
	})
	if err != nil {
		b.Fatalf("NewWithOptions: %v", err)
	}
	b.Cleanup(func() { _ = bc.Close() })
	return bc
}

// benchStoreLegacy constructs a default (append-log-disabled) FSStore so
// WriteAt runs through the tryDirectDiskWrite / memBlock path.
func benchStoreLegacy(b *testing.B) *FSStore {
	b.Helper()
	dir := b.TempDir()
	bc, err := New(dir, 1<<40, 1<<40, nopFBS{})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	b.Cleanup(func() { _ = bc.Close() })
	return bc
}

// BenchmarkAppendWrite_Sequential1GiB measures AppendWrite throughput on a
// 1 GiB sequential write pattern composed of 1 MiB chunks.
//
// b.SetBytes advertises per-op throughput as 1 GiB so go test output
// reports MB/s directly and is trivially comparable across runs.
func BenchmarkAppendWrite_Sequential1GiB(b *testing.B) {
	bc := benchStoreAppend(b)
	ctx := context.Background()
	payload := bytes.Repeat([]byte{0xAA}, benchPayloadLen)
	b.SetBytes(benchTotalBytes)
	b.ResetTimer()
	for b.Loop() {
		for i := 0; i < benchTotalBytes/benchPayloadLen; i++ {
			if err := bc.AppendWrite(ctx, "bench", payload, uint64(i*benchPayloadLen)); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkTryDirectDiskWrite_Sequential1GiB measures the legacy WriteAt /
// tryDirectDiskWrite path on the same 1 GiB sequential workload. Name
// references tryDirectDiskWrite because that is the legacy hot-path sub-
// function being compared against (per D-40 baseline language).
func BenchmarkTryDirectDiskWrite_Sequential1GiB(b *testing.B) {
	bc := benchStoreLegacy(b)
	ctx := context.Background()
	payload := bytes.Repeat([]byte{0xAA}, benchPayloadLen)
	b.SetBytes(benchTotalBytes)
	b.ResetTimer()
	for b.Loop() {
		for i := 0; i < benchTotalBytes/benchPayloadLen; i++ {
			if err := bc.WriteAt(ctx, "bench", payload, uint64(i*benchPayloadLen)); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// medianNs returns the median ns/op from a slice of benchmark results.
// For an even-length slice we pick the upper-middle value (simple and
// deterministic; matches Go's benchstat convention well enough for trend
// capture at 5 samples).
func medianNs(results []int64) int64 {
	sorted := append([]int64(nil), results...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

// TestAppendWriteWithin15pct_D40 enforces the D-40 perf gate.
//
// Design (Warning 4 fix in 10-11-PLAN.md must-haves):
//   - Run each benchmark 5 times with testing.Benchmark (b.N auto-tuned by
//     the framework) so single-run outliers don't flap the gate.
//   - Compute MEDIAN ns/op of each series, compare the medians.
//   - Loosened gate: AppendWrite median must be at most 1.15 * legacy
//     median (was 5% in the original D-40 spec — see the file-level
//     doc comment).
//   - Skipped under -short AND when D40_GATE is unset so normal CI lanes
//     stay green. See test/e2e/BENCHMARKS.md for local invocation.
func TestAppendWriteWithin15pct_D40(t *testing.T) {
	if testing.Short() {
		t.Skip("D-40 gate allocates 1 GiB per iteration; skip under -short")
	}
	if os.Getenv("D40_GATE") == "" {
		t.Skip("D-40 gate is a dev-machine trend gate; set D40_GATE=1 to run")
	}

	const runs = 5
	appendRuns := make([]int64, 0, runs)
	legacyRuns := make([]int64, 0, runs)
	for i := 0; i < runs; i++ {
		ar := testing.Benchmark(BenchmarkAppendWrite_Sequential1GiB)
		lr := testing.Benchmark(BenchmarkTryDirectDiskWrite_Sequential1GiB)
		if ar.NsPerOp() == 0 || lr.NsPerOp() == 0 {
			t.Fatalf("iter %d produced zero ns/op: append=%v legacy=%v", i, ar, lr)
		}
		appendRuns = append(appendRuns, ar.NsPerOp())
		legacyRuns = append(legacyRuns, lr.NsPerOp())
	}

	appendMed := medianNs(appendRuns)
	legacyMed := medianNs(legacyRuns)
	limit := float64(legacyMed) * 1.15
	ratio := float64(appendMed) / float64(legacyMed)

	t.Logf("D-40 medians over %d runs: append=%d ns/op legacy=%d ns/op ratio=%.2f (limit 1.15)",
		runs, appendMed, legacyMed, ratio)

	if float64(appendMed) > limit {
		t.Fatalf("D-40 gate failed: AppendWrite median=%d ns/op legacy median=%d ns/op ratio=%.2f; want <= 1.15",
			appendMed, legacyMed, ratio)
	}
	t.Logf("D-40 gate met: ratio=%.2f <= 1.15", ratio)
}
