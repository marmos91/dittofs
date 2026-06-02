package blockstore

import (
	"context"
	"math/rand/v2"
	"sync"
	"testing"

	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// TestRunWorkerPool_CoversEveryOpOnce asserts the fan-out hands every op index
// in [0,ops) to exactly one worker regardless of worker count, so the op set
// is a pure function of (ops, workers) — the determinism guarantee the storm
// and concurrent workloads rely on.
func TestRunWorkerPool_CoversEveryOpOnce(t *testing.T) {
	const ops = 1000
	for _, workers := range []int{1, 3, 8, 16} {
		var (
			mu   sync.Mutex
			seen = make([]int, ops)
		)
		err := runWorkerPool(context.Background(), workers, ops, 5, func(_ int, _ *rand.Rand, op int) error {
			mu.Lock()
			seen[op]++
			mu.Unlock()
			return nil
		})
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		for i, c := range seen {
			if c != 1 {
				t.Fatalf("workers=%d: op %d ran %d times, want 1", workers, i, c)
			}
		}
	}
}

// TestWorkerRNG_DeterministicAndDistinct verifies per-worker streams reproduce
// across runs (same seed → same draws) but differ between workers (no two
// workers draw the same sequence), which is what keeps a concurrent run both
// replayable and free of accidental cross-worker dedup.
func TestWorkerRNG_DeterministicAndDistinct(t *testing.T) {
	draw := func(seed uint64, worker int) [8]uint64 {
		rng := workerRNG(seed, worker)
		var out [8]uint64
		for i := range out {
			out[i] = rng.Uint64()
		}
		return out
	}
	w0a, w0b, w1 := draw(42, 0), draw(42, 0), draw(42, 1)
	if w0a != w0b {
		t.Error("same (seed, worker) must reproduce the same stream")
	}
	if w0a == w1 {
		t.Error("distinct workers must draw distinct streams")
	}
}

// TestRunConcurrent_WriteAndRead drives each (a)–(d) concurrent workload at a
// small op count and asserts it completes, moves bytes, and tallies ops.
func TestRunConcurrent_WriteAndRead(t *testing.T) {
	for _, wl := range []string{
		WorkloadConcurrentSmallWrite, WorkloadConcurrentSmallRead,
	} {
		t.Run(wl, func(t *testing.T) {
			bs, closeFn, err := NewEngine(t.TempDir(), remotememory.New())
			if err != nil {
				t.Fatalf("NewEngine: %v", err)
			}
			defer closeFn()

			res, err := RunConcurrent(context.Background(), bs, Opts{
				Workload: wl,
				Ops:      120,
				Workers:  4,
				Seed:     3,
			})
			if err != nil {
				t.Fatalf("RunConcurrent: %v", err)
			}
			if res.Ops != 120 {
				t.Errorf("Ops = %d, want 120", res.Ops)
			}
			if res.Bytes <= 0 {
				t.Errorf("Bytes = %d, want > 0", res.Bytes)
			}
			if res.Duration <= 0 {
				t.Errorf("Duration = %v, want > 0", res.Duration)
			}
		})
	}
}

// TestRunConcurrent_RejectsNonConcurrent guards the dispatcher: a serial
// workload name must not be accepted by RunConcurrent.
func TestRunConcurrent_RejectsNonConcurrent(t *testing.T) {
	bs, closeFn, err := NewEngine(t.TempDir(), remotememory.New())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer closeFn()
	if _, err := RunConcurrent(context.Background(), bs, Opts{
		Workload: WorkloadSequentialWrite, Ops: 1, Workers: 1,
	}); err == nil {
		t.Fatal("expected error for non-concurrent workload")
	}
}

// TestParseStormMix covers the default, a valid override, and the malformed
// cases (wrong arity, negative, non-numeric, all-zero).
func TestParseStormMix(t *testing.T) {
	if got, err := parseStormMix(""); err != nil || got != defaultStormWeights() {
		t.Fatalf("empty mix = %+v, %v; want default", got, err)
	}
	if got, err := parseStormMix("70,20,5,5"); err != nil ||
		got != (stormWeights{write: 70, read: 20, list: 5, delete: 5}) {
		t.Fatalf("valid mix = %+v, %v", got, err)
	}
	for _, bad := range []string{"1,2,3", "1,2,3,4,5", "1,-2,3,4", "a,b,c,d", "0,0,0,0"} {
		if _, err := parseStormMix(bad); err == nil {
			t.Errorf("mix %q: expected error", bad)
		}
	}
}

// TestRunStorm_MixHonored runs an all-writes mix and asserts no reads/lists/
// deletes are tallied, proving --mix flows through to op selection.
func TestRunStorm_MixHonored(t *testing.T) {
	bs, closeFn, err := NewEngine(t.TempDir(), remotememory.New())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer closeFn()
	res, err := RunStorm(context.Background(), bs, Opts{
		Workload: WorkloadMixedOpStorm, Ops: 200, BlockSize: 4096,
		WorkingSet: 4, Workers: 2, Seed: 11, Mix: "100,0,0,0",
	})
	if err != nil {
		t.Fatalf("RunStorm: %v", err)
	}
	if res.Storm.Reads != 0 || res.Storm.Lists != 0 || res.Storm.Deletes != 0 {
		t.Errorf("all-write mix tallied non-writes: %+v", *res.Storm)
	}
	if res.Storm.Writes == 0 {
		t.Error("all-write mix tallied zero writes")
	}
}
