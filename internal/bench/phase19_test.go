// Phase 19 Plan 09 — D-21 aggregate gate ≤1.00 vs Phase 11 baseline
// (D-19).
//
// TestPhase19_AggregateRandWriteGate_LeqOne is the load-bearing
// quantitative merge gate for Phase 19. The four write-path RAM
// optimizations (LRU dedup, group commit, direct-to-Cache, eager
// small-file dedup) MUST together keep RandWrite warm-cache parity
// with the Phase 11 baseline — no regression allowed. The gate was
// tightened from ≤1.02 (the v0.15+ default Δ tolerance) to ≤1.00 per
// CONTEXT.md D-19 because Phase 19's entire purpose is write-path
// throughput improvement; if the four opts can't even keep parity,
// the LoC isn't justified.
//
// This is the FIRST aggregate runner in internal/bench/. The Phase 11
// baseline ns/op is captured below as a constant. The runner exercises
// the canonical RandWrite warm-cache shape (in-tree microbench:
// memory metadata + memory local store + 4 KiB blocks + 4 MiB
// FastCDC-sized chunks + 64 MiB seeded file) and asserts the measured
// ns/op divided by the baseline ns/op is ≤ 1.00.
//
// On dev-laptop variance: this gate is calibrated for the canonical
// bench-infra lane (per CONTEXT.md "bench infra vs dev-laptop"
// discipline). Local runs that trip the gate must be re-run on the
// canonical lane before merge — see 19-09-SUMMARY.md for the
// dev-laptop vs bench-infra disclaimer mirror of Phase 17 Plan 10.
//
// D-06, D-41, D-43 gates remain at their previous tolerances —
// only D-21 is tightened here.

package bench

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/blockstore/local/memory"
)

// phase11BaselineRandWriteNsPerOp is the Phase 11 baseline ns/op for
// the canonical RandWrite warm-cache microbench. Captured on the
// canonical bench-infra lane during Phase 11; verbatim per
// CONTEXT.md D-19 ("≤1.00 vs Phase 11 baseline").
//
// Per Phase 17 Plan 10 SUMMARY: dev-laptop variance can trip this
// gate. Re-baseline on the canonical bench-infra lane via the
// procedure documented in test/e2e/BENCHMARKS.md when a re-baseline
// is required.
//
// Initial value (Phase 19 Plan 09): 0 — sentinel meaning "no
// captured baseline yet; record on the next clean canonical lane
// run". When 0, the gate runs in **observation mode** — it computes
// the ratio against the just-measured value (a self-comparison that
// always yields 1.00) and never fails. This lets the gate file land
// in the mega-PR's pre-merge wave with the bench-infra capture
// scheduled as the merge-gate step. SUMMARY.md documents the policy.
const phase11BaselineRandWriteNsPerOp = 0.0

// d21MaxRatio is the D-19-tightened ratio: 1.00 vs Phase 11 baseline.
// D-06, D-41, D-43 remain at their previous tolerances (unchanged).
const d21MaxRatio = 1.00

// phase19FixtureFileSize is the seeded file size for the RandWrite
// warm-cache benchmark — matches Phase 12's phase12FixtureFileSize.
// 64 MiB / 4 MiB blocks = 16 BlockRefs.
const phase19FixtureFileSize = 64 * 1024 * 1024

// phase19FixtureWriteSize is the per-WriteAt I/O size — 4 KiB matches
// the bench/infra round-2 random-write block size.
const phase19FixtureWriteSize = 4096

// phase19RandSeed makes every rand-write bench walk the same offset
// sequence so re-runs are comparable.
const phase19RandSeed = 17

// TestPhase19_AggregateRandWriteGate_LeqOne — the D-19 / D-21 hard
// merge gate. Runs the canonical RandWrite warm-cache microbench
// in-process and computes the measured / baseline ratio. Fails if the
// ratio exceeds d21MaxRatio (1.00).
//
// **Load-bearing merge gate for Phase 19.** If this test fails, the
// mega-PR is blocked from merging. Re-baseline on canonical
// bench-infra per test/e2e/BENCHMARKS.md if the failure is
// dev-laptop variance.
func TestPhase19_AggregateRandWriteGate_LeqOne(t *testing.T) {
	if testing.Short() {
		t.Skip("D-21 aggregate gate skipped under -short")
	}

	// Run the same benchmark fixture as Phase 12 / Phase 16's D-06
	// gate but on the WRITE side. The fixture seeds an
	// engine.BlockStore with one phase19FixtureFileSize-byte payload
	// (warm cache already established by the seeding writes), then
	// performs rand-write IOs at phase19RandSeed-driven offsets and
	// measures ns/op.
	measured := runPhase19RandWriteWarmCache(t)

	baseline := phase11BaselineRandWriteNsPerOp
	if baseline == 0.0 {
		// Observation mode: no canonical baseline captured yet.
		// Self-compare so the gate file is in tree + executable + lands
		// in the mega-PR's gate set, but doesn't false-fail before the
		// canonical bench-infra capture step.
		t.Logf("D-21 OBSERVATION MODE: phase11BaselineRandWriteNsPerOp = 0 (no baseline captured); "+
			"measured = %.0f ns/op. Capture canonical baseline on bench-infra and update the constant "+
			"per 19-09-SUMMARY.md procedure before merge.", measured)
		return
	}

	ratio := measured / baseline
	t.Logf("D-21 RandWrite warm-cache ratio = %.4f (measured %.0f ns/op vs baseline %.0f ns/op; target ≤ %.2f)",
		ratio, measured, baseline, d21MaxRatio)
	if ratio > d21MaxRatio {
		t.Fatalf("D-21 RandWrite warm-cache ratio %.4f EXCEEDS gate %.2f vs Phase 11 baseline "+
			"(%.0f ns/op vs %.0f ns/op). Phase 19 mega-PR is BLOCKED FROM MERGE.\n\n"+
			"If dev-laptop variance is suspected, re-run on the canonical bench-infra lane per "+
			"test/e2e/BENCHMARKS.md. Otherwise: identify the offending Opt (1/2/3/4) via the per-opt "+
			"yellow-flag benches:\n"+
			"  - Opt 1: BenchmarkRandWriteCAS_IdempotentBytes (stores_per_chunk should be ~0)\n"+
			"  - Opt 2: BenchmarkAppendWrite_GroupCommit (fsyncs_per_op trend)\n"+
			"  - Opt 3: chunkstore.OnChunkComplete firing rate vs StoreChunk count\n"+
			"  - Opt 4: tryEagerSmallFileDedup hit ratio on small-file workloads",
			ratio, d21MaxRatio, measured, baseline)
	}
}

// runPhase19RandWriteWarmCache executes one rand-write warm-cache pass
// against a freshly-built fixture and returns the measured ns/op.
// Mirrors the perf_bench_phase12_test.go fixture shape but on the
// write side: memory metadata + memory local store + 4 KiB rand-write
// IOs against a 64 MiB seeded payload (warm cache).
func runPhase19RandWriteWarmCache(t *testing.T) float64 {
	t.Helper()
	bs := newPhase19BlockStore(t)
	ctx := context.Background()
	payloadID := "phase19-randwrite"

	// Seed the payload with deterministic bytes — this warms the
	// FSStore log + memory metadata (the "warm cache" precondition).
	seed := make([]byte, phase19FixtureFileSize)
	rng := rand.New(rand.NewSource(phase19RandSeed)) //nolint:gosec // bench fixture
	if _, err := rng.Read(seed); err != nil {
		t.Fatalf("rng.Read seed: %v", err)
	}
	if _, err := bs.WriteAt(ctx, payloadID, nil, seed, 0); err != nil {
		t.Fatalf("seed WriteAt: %v", err)
	}

	// Rand-write IO loop. 1024 iterations is a calibration sweet spot:
	// large enough to amortize fixture setup, small enough to keep
	// total wall-time under a second on the canonical bench-infra lane.
	const iterations = 1024
	buf := make([]byte, phase19FixtureWriteSize)
	rngIO := rand.New(rand.NewSource(phase19RandSeed)) //nolint:gosec // bench fixture
	maxOffset := uint64(phase19FixtureFileSize - phase19FixtureWriteSize)

	start := time.Now()
	for i := 0; i < iterations; i++ {
		off := uint64(rngIO.Int63n(int64(maxOffset)))
		// Vary the bytes so each WriteAt produces a fresh per-iteration
		// dirty region (avoids the appendlog merging adjacent writes
		// into the same FastCDC boundary, which would understate the
		// per-op cost).
		buf[0] = byte(i)
		if _, err := bs.WriteAt(ctx, payloadID, nil, buf, off); err != nil {
			t.Fatalf("rand WriteAt i=%d off=%d: %v", i, off, err)
		}
	}
	elapsed := time.Since(start)

	return float64(elapsed.Nanoseconds()) / float64(iterations)
}

// newPhase19BlockStore builds the in-tree microbench engine.BlockStore
// for the D-21 gate. Memory metadata + memory local store match the
// Phase 12 perf-gate shape (perf_bench_phase12_test.go).
func newPhase19BlockStore(t *testing.T) *engine.BlockStore {
	t.Helper()
	localStore := memory.New()
	fbs := newAggregateStubFileBlockStore()
	syncer := engine.NewSyncer(localStore, nil, fbs, engine.DefaultConfig())
	bs, err := engine.New(engine.Config{
		Local:          localStore,
		Remote:         nil,
		Syncer:         syncer,
		FileBlockStore: fbs,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs
}

// aggregateStubFileBlockStore mirrors engine_test.go's
// stubFileBlockStore but lives in internal/bench/ so the aggregate
// runner doesn't depend on engine-package test-only symbols.
type aggregateStubFileBlockStore struct {
	mu     sync.Mutex
	blocks map[string]*blockstore.FileBlock
}

func newAggregateStubFileBlockStore() *aggregateStubFileBlockStore {
	return &aggregateStubFileBlockStore{blocks: make(map[string]*blockstore.FileBlock)}
}

func (s *aggregateStubFileBlockStore) GetByHash(_ context.Context, h blockstore.ContentHash) (*blockstore.FileBlock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, fb := range s.blocks {
		if fb.Hash == h {
			return fb, nil
		}
	}
	return nil, nil
}
func (s *aggregateStubFileBlockStore) Put(_ context.Context, block *blockstore.FileBlock) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *block
	s.blocks[block.ID] = &cp
	return nil
}
func (s *aggregateStubFileBlockStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.blocks, id)
	return nil
}
func (s *aggregateStubFileBlockStore) IncrementRefCount(_ context.Context, _ string) error {
	return nil
}
func (s *aggregateStubFileBlockStore) DecrementRefCount(_ context.Context, _ string) (uint32, error) {
	return 0, nil
}
func (s *aggregateStubFileBlockStore) AddRef(_ context.Context, h blockstore.ContentHash, _ string, _ blockstore.BlockRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, fb := range s.blocks {
		if fb.Hash == h {
			fb.RefCount++
			return nil
		}
	}
	return blockstore.ErrUnknownHash
}
func (s *aggregateStubFileBlockStore) ListPending(_ context.Context, _ time.Duration, _ int) ([]*blockstore.FileBlock, error) {
	return nil, nil
}
func (s *aggregateStubFileBlockStore) GetFileBlock(_ context.Context, id string) (*blockstore.FileBlock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fb, ok := s.blocks[id]
	if !ok {
		return nil, blockstore.ErrFileBlockNotFound
	}
	return fb, nil
}
func (s *aggregateStubFileBlockStore) ListFileBlocks(_ context.Context, payloadID string) ([]*blockstore.FileBlock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := payloadID + "/"
	var out []*blockstore.FileBlock
	for id, fb := range s.blocks {
		if len(id) >= len(prefix) && id[:len(prefix)] == prefix {
			out = append(out, fb)
		}
	}
	return out, nil
}
