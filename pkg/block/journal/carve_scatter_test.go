package journal

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestCarveScatteredRunsPackBySize pins that a scattered dirty set costs remote
// objects in proportion to its bytes, not to its number of dirty ranges. Each
// 4 KiB write lands in its own non-contiguous run; cutting a block per run would
// commit one block — one remote round-trip — per write, so a randomly written
// file would drain at one round-trip per 4 KiB however few bytes were dirty.
func TestCarveScatteredRunsPackBySize(t *testing.T) {
	const (
		runs      = 200
		runSize   = 4 << 10
		gap       = 8 << 10 // stride > runSize keeps every run non-contiguous
		blockSize = 4 << 20
	)
	totalBytes := int64(runs * runSize)
	s, _, sink, _ := carveStore(t, Config{CarveBlockSize: blockSize})
	ctx := context.Background()

	for i := 0; i < runs; i++ {
		if err := s.WriteAt(ctx, "f", int64(i)*gap, randBytes(runSize, int64(i))); err != nil {
			t.Fatalf("WriteAt %d: %v", i, err)
		}
	}
	if s.UnsyncedBytes() != totalBytes {
		t.Fatalf("pre-carve unsynced=%d want %d", s.UnsyncedBytes(), totalBytes)
	}

	res, err := s.Carve(ctx, CarveOptions{Force: true})
	if err != nil {
		t.Fatalf("Carve: %v", err)
	}

	wantMax := int((totalBytes + blockSize - 1) / blockSize)
	if got := sink.blockCount(); got > wantMax {
		t.Fatalf("committed %d blocks for %d KiB across %d runs, want <= %d — "+
			"blocks are being cut per run rather than per CarveBlockSize",
			got, totalBytes>>10, runs, wantMax)
	}
	if res.BytesCarved != totalBytes {
		t.Fatalf("BytesCarved=%d want %d", res.BytesCarved, totalBytes)
	}
	if s.UnsyncedBytes() != 0 {
		t.Fatalf("post-carve unsynced=%d want 0", s.UnsyncedBytes())
	}
}

// TestCarveScatteredRunsCommitsOverlap pins that commits still overlap when the
// blocks come from different runs. Commits are remote round-trips, so overlap
// decides whether draining N blocks costs N latencies or N/concurrency; waiting
// for each run's commits before starting the next serializes them completely,
// however much concurrency the dispatcher is given.
func TestCarveScatteredRunsCommitsOverlap(t *testing.T) {
	const (
		runs      = 64
		runSize   = 4 << 10
		gap       = 8 << 10
		blockSize = 16 << 10 // small, so the scattered set spans several blocks
	)
	s, _, sink, _ := carveStore(t, Config{CarveBlockSize: blockSize, CarveUploadConcurrency: 4})
	ctx := context.Background()

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	sink.onCommit = func([]CarveChunk) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		// Hold the commit open so a genuinely overlapping one is observable.
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
	}

	for i := 0; i < runs; i++ {
		if err := s.WriteAt(ctx, "f", int64(i)*gap, randBytes(runSize, int64(i))); err != nil {
			t.Fatalf("WriteAt %d: %v", i, err)
		}
	}
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve: %v", err)
	}

	mu.Lock()
	gotPeak, gotBlocks := peak, sink.blockCount()
	mu.Unlock()

	if gotBlocks < 2 {
		t.Fatalf("only %d block(s) committed; the fixture needs several to show overlap", gotBlocks)
	}
	if gotPeak < 2 {
		t.Fatalf("peak concurrent commits = %d across %d blocks, want >= 2 — "+
			"commits are serialized at run boundaries", gotPeak, gotBlocks)
	}
}
