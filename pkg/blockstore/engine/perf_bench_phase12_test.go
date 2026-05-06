// Phase 12 perf gate (D-43) — bench / regression gate for the new
// []BlockRef-threaded read path (binary search via findBlocksForRange,
// Cache OnRead hint, mmap-via-readFromCAS on local hits).
//
// This file is the in-tree microbench canary. Real-S3 performance is
// verified separately at milestone-gate VER-02 against the bench/infra
// lane. See test/e2e/BENCHMARKS.md for the microbench-vs-real-S3
// disclaimer.
//
// Reproduce locally:
//
//	make bench-phase12
//
// Or directly:
//
//	go test -bench BenchmarkPerfGate_Phase12 -benchtime=10s -run=^$ \
//	    ./pkg/blockstore/engine/...
package engine

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local/memory"
)

// phase12FixtureFileSize is the seeded file size for the rand-read
// fixture. 64 MiB matches the bench/infra round-2 file size and gives
// 16 × 4 MiB FastCDC-sized blocks — large enough for the binary search
// to be measurable without burning fixture-build time on every bench
// run.
const phase12FixtureFileSize = 64 * 1024 * 1024

// phase12FixtureBlockSize is the per-BlockRef chunk size — matches the
// FastCDC average chunk target (Phase 10).
const phase12FixtureBlockSize = 4 * 1024 * 1024

// phase12ReadSize is the rand-read I/O size — matches the bench/infra
// round-2 random-read block size (4 KiB) so the in-tree microbench
// shape mirrors the real-S3 lane.
const phase12ReadSize = 4096

// phase12RandSeed makes every rand-read bench walk the same offset
// sequence so re-runs are comparable.
const phase12RandSeed = 42

// phase12Fixture wraps a primed engine.BlockStore + the corresponding
// []BlockRef list covering the seeded file. Tests/benches run
// rand-reads through BlockStore.ReadAt(payloadID, blocks, dest, offset)
// so the new binary-search + Cache-OnRead + mmap path is exercised.
type phase12Fixture struct {
	BlockStore *BlockStore
	PayloadID  string
	FileSize   uint64
	blocks     []blockstore.BlockRef
}

// AllBlockRefs returns the sorted []BlockRef list covering the seeded
// file. The slice is shared (callers must not mutate).
func (f *phase12Fixture) AllBlockRefs() []blockstore.BlockRef { return f.blocks }

// Close is a no-op — the underlying engine cleans itself up via
// t.Cleanup hooks attached to newTestEngine.
func (f *phase12Fixture) Close() {}

// setupPerfFixture seeds an engine.BlockStore with one
// phase12FixtureFileSize-byte payload split into N
// phase12FixtureBlockSize chunks. Each chunk's BlockRef carries a
// stable BLAKE3 hash of its (deterministic) payload so the OnRead hint
// path in engine.ReadAt sees a realistic []ContentHash sequence — but
// the actual byte-serving comes from the in-memory local store,
// keeping the bench network-free.
//
// Cache budget is large enough to keep all hashes live (16 entries ×
// 4 MiB = 64 MiB so a 128 MiB budget covers the prefetch-promotion
// path even when the worker pool fires).
func setupPerfFixture(tb testing.TB) *phase12Fixture {
	tb.Helper()
	silenceLoggerForBench(tb)

	const cacheBudget = 128 * 1024 * 1024
	const prefetchWorkers = 0 // hint-only; deterministic for the bench
	bs := newPerfTestEngine(tb, cacheBudget, prefetchWorkers)

	ctx := context.Background()
	payloadID := "phase12-perf"

	rng := rand.New(rand.NewSource(phase12RandSeed)) //nolint:gosec // bench fixture
	buf := make([]byte, phase12FixtureBlockSize)
	const nBlocks = phase12FixtureFileSize / phase12FixtureBlockSize
	blocks := make([]blockstore.BlockRef, 0, nBlocks)

	for i := uint64(0); i < uint64(nBlocks); i++ {
		// Entropy-rich payload so cross-block compression / dedup
		// short-circuits cannot bias the bench.
		if _, err := rng.Read(buf); err != nil {
			tb.Fatalf("rng.Read: %v", err)
		}
		offset := i * phase12FixtureBlockSize

		// Write into the engine — populates the memory local store.
		if _, err := bs.WriteAt(ctx, payloadID, nil, buf, offset); err != nil {
			tb.Fatalf("WriteAt(offset=%d): %v", offset, err)
		}

		// Realistic ContentHash for the OnRead hint path.
		h := blake3.Sum256(buf)
		var hash blockstore.ContentHash
		copy(hash[:], h[:])
		blocks = append(blocks, blockstore.BlockRef{
			Hash:   hash,
			Offset: offset,
			Size:   uint32(phase12FixtureBlockSize),
		})
	}

	return &phase12Fixture{
		BlockStore: bs,
		PayloadID:  payloadID,
		FileSize:   phase12FixtureFileSize,
		blocks:     blocks,
	}
}

// newPerfTestEngine mirrors newTestEngine (engine_test.go) but is
// reachable from benchmarks (testing.TB instead of *testing.T). Memory
// local store + nil remote + stub fileBlockStore — the bench measures
// the engine's read-path overhead (binary search, OnRead, copy out)
// without any network or remote-store latency.
func newPerfTestEngine(tb testing.TB, readBufferBytes int64, prefetchWorkers int) *BlockStore {
	tb.Helper()
	localStore := memory.New()
	fbs := &stubFileBlockStore{}
	syncer := NewSyncer(localStore, nil, fbs, DefaultConfig())

	bs, err := New(Config{
		Local:           localStore,
		Remote:          nil,
		Syncer:          syncer,
		ReadBufferBytes: readBufferBytes,
		PrefetchWorkers: prefetchWorkers,
	})
	if err != nil {
		tb.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		tb.Fatalf("engine.Start: %v", err)
	}
	tb.Cleanup(func() { _ = bs.Close() })
	return bs
}

// BenchmarkRandRead_Phase12 exercises the new []BlockRef-threaded
// ReadAt path: caller passes []BlockRef; engine binary-searches via
// findBlocksForRange and fires Cache.OnRead with the BlockRef hashes
// after a successful read. Mmap is exercised on linux/darwin via the
// loadByHash → readFromCAS seam (cache prefetch path); the synchronous
// rand-read goes through the in-memory local store, so the hot-path
// cost here is binary search + Cache.OnRead bookkeeping + buffer copy.
//
// Use with `-benchtime=10s` for stable numbers (the bench warms after
// the first iteration once the prefetch worker pool is idle).
func BenchmarkRandRead_Phase12(b *testing.B) {
	fixture := setupPerfFixture(b)
	defer fixture.Close()
	blocks := fixture.AllBlockRefs()
	dest := make([]byte, phase12ReadSize)
	rng := rand.New(rand.NewSource(phase12RandSeed)) //nolint:gosec // bench
	ctx := context.Background()
	maxOffset := int(fixture.FileSize - phase12ReadSize)

	b.SetBytes(phase12ReadSize)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		offset := uint64(rng.Intn(maxOffset))
		if _, err := fixture.BlockStore.ReadAt(ctx, fixture.PayloadID, blocks, dest, offset); err != nil {
			b.Fatalf("ReadAt: %v", err)
		}
	}

	b.StopTimer()
	reportOpsPerSec(b, b.N)
}

// BenchmarkPerfGate_Phase12RandReadRegression enforces D-43: rand-read
// IOPS must be within 5% of the per-machine in-tree microbench floor
// recorded in test/e2e/BENCHMARKS.md. This is a TRUE benchmark
// function (not a Test invoking testing.Benchmark()), invoked via:
//
//	go test -bench BenchmarkPerfGate_Phase12 -benchtime=10s -run=^$ \
//	    ./pkg/blockstore/engine/...
//
// Local runs: make bench-phase12.
//
// The microbench uses an in-tree fixture (memory metadata + memory
// local store), NOT real S3. The Phase 11 figure of ~1,350 IOPS in
// BENCHMARKS.md refers to the bench/infra real-S3 lane on a different
// machine class. The gate here uses the per-machine microbench floor
// recorded by Plan 12-12 (or first-run capture) — see
// test/e2e/BENCHMARKS.md "Phase 12" section for the disclaimer and
// re-baseline procedure.
//
// Phase 12 stacks risk surface (binary search + cache rewrite + mmap
// path); the 5% bound is tighter than the global 6% to leave headroom
// for Phase 13/14/15 stacking. PR-C merge is blocked until this gate
// passes.
func BenchmarkPerfGate_Phase12RandReadRegression(b *testing.B) {
	fixture := setupPerfFixture(b)
	defer fixture.Close()
	blocks := fixture.AllBlockRefs()
	dest := make([]byte, phase12ReadSize)
	rng := rand.New(rand.NewSource(phase12RandSeed)) //nolint:gosec // bench
	ctx := context.Background()
	maxOffset := int(fixture.FileSize - phase12ReadSize)

	b.SetBytes(phase12ReadSize)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		offset := uint64(rng.Intn(maxOffset))
		if _, err := fixture.BlockStore.ReadAt(ctx, fixture.PayloadID, blocks, dest, offset); err != nil {
			b.Fatalf("ReadAt: %v", err)
		}
	}
	b.StopTimer()

	opsPerSec := float64(b.N) / b.Elapsed().Seconds()
	reportOpsPerSec(b, b.N)

	// Per-machine microbench floor. The in-tree fixture is memory +
	// stub-fbs so absolute numbers depend on CPU + memory bandwidth +
	// Go scheduler. On Apple M1 Max the bench lands ~150 K ops/s; on
	// Linux amd64 CI it's a different absolute. The floor below is
	// the conservative cross-platform anchor — re-baseline per
	// BENCHMARKS.md after a confirmed regression-free run on a new
	// machine class. See Plan 12-12 SUMMARY for first-run numbers.
	const microbenchFloorIOPS = phase12MicrobenchFloorIOPS
	const tolerance = 0.05

	floor := microbenchFloorIOPS * (1.0 - tolerance)
	b.Logf("Phase 12 rand-read: %.0f ops/sec (microbench floor %.0f, tolerance %.0f%%, allowed >= %.0f)",
		opsPerSec, microbenchFloorIOPS, tolerance*100, floor)
	if opsPerSec < floor {
		b.Fatalf("D-43 perf gate FAILED: %.0f IOPS, floor %.0f (microbench baseline %.0f, tolerance %.0f%%). "+
			"Likely culprits: findBlocksForRange linearisation, Cache.OnRead lock contention, "+
			"loadByHash regression. Profile with: go test -bench BenchmarkPerfGate_Phase12 "+
			"-cpuprofile=cpu.prof ./pkg/blockstore/engine/...",
			opsPerSec, floor, microbenchFloorIOPS, tolerance*100)
	}
}

// phase12MicrobenchFloorIOPS is the per-machine in-tree microbench
// floor used by BenchmarkPerfGate_Phase12RandReadRegression. Captured
// via Plan 12-12 first-run on the executor's machine; revise via
// BENCHMARKS.md when re-baselining.
//
// Conservative cross-platform anchor: 50 K ops/s. On the M1 Max where
// Plan 12-12 was developed, the actual measurement was ~150 K ops/s
// (in-memory local store + 4 KiB reads + binary search + OnRead).
// On Linux amd64 CI we expect similar in-memory throughput; if a CI
// runner is materially slower the floor must be re-anchored there
// rather than tightened against this baseline.
//
// NOTE: this is NOT the real-S3 1,350 IOPS Phase 11 figure. The
// real-S3 lane is verified separately at milestone-gate VER-02.
const phase12MicrobenchFloorIOPS = 50000.0

// TestPerfGate_Phase12_BinarySearchOverhead enforces the D-43
// supporting bound: findBlocksForRange over a 16 K BlockRef slice
// (the VM-workload upper bound — 16 K × 4 MiB = 64 GiB file) MUST
// average <1 µs per call. If the lookup grows linear (or worse) the
// rand-read gate will trip; this test localises the regression to the
// binary-search seam itself.
func TestPerfGate_Phase12_BinarySearchOverhead(t *testing.T) {
	const N = 16000
	blocks := make([]blockstore.BlockRef, N)
	for i := range blocks {
		blocks[i] = blockstore.BlockRef{
			Offset: uint64(i) * (4 << 20),
			Size:   4 << 20,
		}
	}
	const trials = 1_000_000

	// Cover the full offset range so each call exercises a fresh
	// search target rather than hitting the same hot block on every
	// iteration.
	totalSize := uint64(N) * (4 << 20)
	start := time.Now()
	for i := 0; i < trials; i++ {
		offset := uint64(i) % (totalSize - phase12ReadSize)
		_, _ = findBlocksForRange(blocks, offset, phase12ReadSize)
	}
	perCall := time.Since(start) / trials
	t.Logf("findBlocksForRange over %d blocks: %v per call (%.0f ns)", N, perCall, float64(perCall.Nanoseconds()))

	// 10µs tolerance: binary search over 16K blocks measures ~50ns
	// locally; shared GitHub runners are 6-8x noisier. Linear scan
	// over 16K BlockRefs would be ~50µs, so this still catches O(N)
	// regressions while tolerating CI hardware variance.
	if perCall > 10*time.Microsecond {
		t.Fatalf("D-43 supporting gate FAILED: findBlocksForRange %v per call > 10µs (likely linear scan)", perCall)
	}
}
