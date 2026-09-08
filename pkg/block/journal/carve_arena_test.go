package journal

import (
	"context"
	"runtime"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/chunker"
)

// TestCarveArenaSizedToConfiguredChunkSize pins the memory a carve reserves for
// blocks in flight.
//
// Each in-flight block owns a private arena of one CarveBlockSize plus one
// chunk of overhang, because a block is flushed only once it crosses
// CarveBlockSize and so overshoots by the chunk that crossed the line. Sizing
// that overhang from the package ceiling rather than the share's own
// ChunkParams reserves 16 MiB per block for a share chunking at 16 KiB, and the
// reservation is per concurrency slot — so it multiplies by CarveUploadConcurrency
// and again by however many files carve at once.
//
// The arenas come from a sync.Pool, which Go empties on GC, so collecting first
// makes the allocation observable instead of served from a previous test's
// leftovers.
func TestCarveArenaSizedToConfiguredChunkSize(t *testing.T) {
	const (
		blockSize = 1 << 20
		fileSize  = 6 << 20
		window    = 4
	)
	params := chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10}

	s, _, sink, _ := carveStore(t, Config{
		CarveBlockSize:         blockSize,
		CarveUploadConcurrency: window,
		ChunkParams:            params,
	})
	ctx := context.Background()

	if err := s.WriteAt(ctx, "f", 0, randBytes(fileSize, 1)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	runtime.GC()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve: %v", err)
	}
	runtime.ReadMemStats(&after)

	sink.mu.Lock()
	blocks := sink.blocks
	sink.mu.Unlock()
	if blocks < window {
		t.Fatalf("carved %d blocks, need at least %d to fill the concurrency window", blocks, window)
	}

	// What the arenas may cost: one per slot, each a block plus one configured
	// chunk. Everything else the pass allocates — the file's own bytes, the
	// chunker scratch buffer, the manifest rows — is bounded well inside the
	// slack below, and sizing the overhang from chunker.MaxChunkSize instead
	// would put the arenas alone an order of magnitude over it.
	arenas := int64(window) * (blockSize + int64(params.Max))
	limit := arenas + 8*fileSize
	if got := int64(after.TotalAlloc - before.TotalAlloc); got > limit {
		t.Errorf("carve allocated %d bytes, over the %d-byte ceiling for %d slots of %d-byte arenas — "+
			"the per-block overhang is not sized to this share's chunks",
			got, limit, window, blockSize+params.Max)
	}
}
