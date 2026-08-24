package journal

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/chunker"
)

// TestCarvePackReachesBlockSizeOnScatteredRuns pins the defect this plan fixes:
// a scattered dirty set must coalesce into one remote block, not one per run.
// Each run is far below CarveBlockSize, so a packer that flushes at the end of
// every run emits `runs` blocks; one that flushes only at CarveBlockSize emits 1.
func TestCarvePackReachesBlockSizeOnScatteredRuns(t *testing.T) {
	const (
		runs    = 300
		runSize = 4 << 10
		gap     = 64 << 10 // a hole between runs keeps them separate
	)
	s, _, _, _ := carveStore(t, Config{
		CarveBlockSize:         4 << 20,
		CarveUploadConcurrency: 4,
		ChunkParams:            chunker.Params{Min: 1 << 10, Avg: 2 << 10, Max: 8 << 10},
	})
	ctx := context.Background()

	for i := 0; i < runs; i++ {
		if err := s.WriteAt(ctx, "f", int64(i)*gap, randBytes(runSize, int64(i))); err != nil {
			t.Fatalf("WriteAt %d: %v", i, err)
		}
	}

	res, err := s.Carve(ctx, CarveOptions{Force: true})
	if err != nil {
		t.Fatalf("Carve: %v", err)
	}
	if res.BytesCarved != int64(runs*runSize) {
		t.Fatalf("BytesCarved=%d want %d", res.BytesCarved, runs*runSize)
	}
	// 300 x 4 KiB = 1.2 MiB, comfortably inside one 4 MiB block.
	if res.BlocksWritten != 1 {
		t.Fatalf("BlocksWritten=%d want 1: blocks did not span runs", res.BlocksWritten)
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("post-carve unsynced=%d want 0", s.UnsyncedBytes())
	}
}

// TestCarvePackSpansRunsFlipsEveryContributingRun pins that when one block covers
// many runs, every record in every contributing run has its durable synced bit
// set — not merely that the in-memory unsynced counter reached zero. The on-disk
// flag is what recovery reads, so it is the only assertion that rules out the
// silent-zeros class.
func TestCarvePackSpansRunsFlipsEveryContributingRun(t *testing.T) {
	const (
		runs    = 64
		runSize = 4 << 10
		gap     = 32 << 10
	)
	s, _, sink, _ := carveStore(t, Config{
		CarveBlockSize:         4 << 20,
		CarveUploadConcurrency: 4,
		ChunkParams:            chunker.Params{Min: 1 << 10, Avg: 2 << 10, Max: 8 << 10},
	})
	ctx := context.Background()

	want := map[int64][]byte{}
	for i := 0; i < runs; i++ {
		off := int64(i) * gap
		b := randBytes(runSize, int64(i))
		if err := s.WriteAt(ctx, "f", off, b); err != nil {
			t.Fatalf("WriteAt %d: %v", i, err)
		}
		want[off] = b
	}
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve: %v", err)
	}
	for off, b := range want {
		if got := sink.chunkAt(off); got == nil {
			t.Fatalf("no committed chunk at %d", off)
		} else if string(got) != string(b) {
			t.Fatalf("committed bytes at %d differ", off)
		}
		if f := recRawFlags(t, s, "f", off); f&flagSynced == 0 {
			t.Fatalf("record at %d not flipped synced on disk: flags=%#x", off, f)
		}
	}
}

// TestCarvePackFlipPlanArithmetic pins the per-run watermark derivation on
// hand-built runState arrays, with no store and no sink: runs before the last
// flip to their own end, the last flips only to the offset actually packed.
func TestCarvePackFlipPlanArithmetic(t *testing.T) {
	mk := func(off, length int64) *runState {
		return &runState{ivs: []interval{{fileOff: off, length: length}}}
	}
	rs := []*runState{mk(0, 4096), mk(8192, 4096), mk(16384, 4096)}
	plan := flipPlan{first: 0, last: 2, lastOff: 18000}

	var got []int64
	for i := plan.first; i <= plan.last; i++ {
		wm := rs[i].end()
		if i == plan.last {
			wm = plan.lastOff
		}
		got = append(got, wm)
	}
	want := []int64{4096, 12288, 18000}
	if len(got) != len(want) {
		t.Fatalf("watermarks=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("watermark[%d]=%d want %d", i, got[i], want[i])
		}
	}
}

// TestCarvePackSeamRunFailureLeavesSuffixDirty pins the abort semantics at a
// block seam: when a run is split across blocks k and k+1 and k+1's commit
// fails, the run's prefix is durable and flipped while its suffix stays dirty
// for the next pass. This is the same mid-run semantics carve has always had;
// spanning runs must not widen it into a half-flipped run reported as complete.
//
// The run is laid down as many adjacent writes rather than one large one: a
// record is the granularity flipUpTo advances at, so a single 512 KiB write
// would be one interval that no mid-run watermark can flip.
func TestCarvePackSeamRunFailureLeavesSuffixDirty(t *testing.T) {
	const runSize = 512 << 10
	s, _, sink, _ := carveStore(t, Config{
		CarveBlockSize:         64 << 10,
		CarveUploadConcurrency: 1,
		ChunkParams:            chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10},
	})
	writeAdjacent(t, s, "f", 128, 4<<10)
	ctx := context.Background()
	// Let the first block commit, then fail every one after it. fakeSink already
	// has both hooks — do not add a new failure field.
	sink.okCommits = 1
	sink.failErr = errors.New("seam commit failed")

	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err == nil {
		t.Fatal("Carve: want the seam failure to surface, got nil")
	}
	if s.UnsyncedBytes() == 0 {
		t.Fatal("post-carve unsynced=0: the failed suffix must stay dirty")
	}
	if s.UnsyncedBytes() == int64(runSize) {
		t.Fatal("post-carve unsynced=runSize: the committed prefix must have flipped")
	}
}
