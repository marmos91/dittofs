package journal

import (
	"context"
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
