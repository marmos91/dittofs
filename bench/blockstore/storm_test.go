package blockstore

import (
	"context"
	"testing"

	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
)

// TestRunStorm_Concurrent exercises the storm at single- and multi-worker
// concurrency with a tiny op count, asserting it completes, issues at least one
// of each common op type, and never tallies more ops than requested.
func TestRunStorm_Concurrent(t *testing.T) {
	cases := []struct {
		name    string
		workers int
	}{
		{"serial", 1},
		{"parallel", 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bs, closeFn, err := NewEngine(t.TempDir(), remotememory.New())
			if err != nil {
				t.Fatalf("NewEngine: %v", err)
			}
			defer closeFn()

			const ops = 400
			res, err := RunStorm(context.Background(), bs, Opts{
				Workload:   WorkloadMixedOpStorm,
				Ops:        ops,
				BlockSize:  4 * 1024,
				WorkingSet: 4,
				Workers:    tc.workers,
				Seed:       7,
			})
			if err != nil {
				t.Fatalf("RunStorm: %v", err)
			}
			if res.Storm == nil {
				t.Fatal("Storm counts are nil")
			}
			sum := res.Storm.Writes + res.Storm.Reads + res.Storm.Lists + res.Storm.Deletes
			// Reads/deletes can be skipped (empty set / delete floor), so the
			// executed total may be <= ops, but never more.
			if sum <= 0 || sum > ops {
				t.Errorf("executed op sum = %d, want in (0, %d]", sum, ops)
			}
			if res.Storm.Writes == 0 {
				t.Error("expected at least one write")
			}
			if res.Duration <= 0 {
				t.Errorf("Duration = %v, want > 0", res.Duration)
			}
		})
	}
}

// TestRunStorm_DeterministicAtSingleWorker verifies that with a fixed seed and
// one worker the op multiset is reproducible: op types and skip decisions are
// driven by a single PRNG, so two runs tally identically. (At workers>1
// goroutine interleaving makes only the shape, not the exact counts, stable.)
func TestRunStorm_DeterministicAtSingleWorker(t *testing.T) {
	run := func() StormCounts {
		bs, closeFn, err := NewEngine(t.TempDir(), remotememory.New())
		if err != nil {
			t.Fatalf("NewEngine: %v", err)
		}
		defer closeFn()
		res, err := RunStorm(context.Background(), bs, Opts{
			Workload:   WorkloadMixedOpStorm,
			Ops:        300,
			BlockSize:  4 * 1024,
			WorkingSet: 4,
			Workers:    1,
			Seed:       42,
		})
		if err != nil {
			t.Fatalf("RunStorm: %v", err)
		}
		return *res.Storm
	}
	a, b := run(), run()
	if a != b {
		t.Errorf("non-deterministic counts at workers=1: %+v vs %+v", a, b)
	}
}

// TestRunStorm_RejectsBadBlockSize guards the offset-range invariant: a block
// larger than a storm file would make every write/read offset illegal.
func TestRunStorm_RejectsBadBlockSize(t *testing.T) {
	bs, closeFn, err := NewEngine(t.TempDir(), remotememory.New())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer closeFn()
	if _, err := RunStorm(context.Background(), bs, Opts{
		Workload:  WorkloadMixedOpStorm,
		Ops:       1,
		BlockSize: StormFileSize + 1,
		Workers:   1,
	}); err == nil {
		t.Fatal("expected error for block size > StormFileSize")
	}
}
