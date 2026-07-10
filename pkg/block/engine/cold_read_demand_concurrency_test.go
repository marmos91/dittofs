package engine

import (
	"context"
	"testing"
	"time"

	memorylocal "github.com/marmos91/dittofs/pkg/block/local/memory"
	metastore "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestColdRead_DemandFetchIsConcurrent pins the cold-read fix: a single read
// spanning many not-local blocks must fetch them from the remote CONCURRENTLY,
// not one blocking S3 GET per block. Before the fix the demand loop in
// EnsureAvailableAndRead was serial, which pinned cold-read throughput at
// blockSize/latency (one GET per round-trip — the ~159 MB/s wall).
//
// The assertion is STRUCTURAL, not wall-clock timing: the latency-injecting
// remote records the peak number of GETs in flight at once. Serial fetching
// pins that at 1; the bounded parallel fan-out drives it up to nBlocks. This
// keeps the test deterministic and free of timing flakes.
func TestColdRead_DemandFetchIsConcurrent(t *testing.T) {
	const nBlocks = 8
	ctx := context.Background()

	loc := memorylocal.New()
	rs := newLatencyRemote(20 * time.Millisecond) // injected per-GET WAN latency
	fbs := newStubFileChunkStore()
	mds := metastore.NewMemoryMetadataStoreWithDefaults()

	// Seed nBlocks remote-only chunks, one per block stride, so a single read
	// over [0, nBlocks*BlockSize) must fetch every one from the remote. Each
	// block gets DISTINCT bytes (distinct CAS hash) — identical content would
	// dedup to local after the first fetch and hide the serial-vs-parallel
	// difference behind CAS, not the loop.
	for i := 0; i < nBlocks; i++ {
		chunk := make([]byte, 4096)
		for j := range chunk {
			chunk[j] = byte(i*7 + j)
		}
		seedSyncedRemoteChunk(t, fbs, rs, mds, "p", uint64(i)*uint64(BlockSize), chunk)
	}

	m := newFetchSyncer(loc, rs, fbs, mds)
	m.config.PrefetchBlocks = 0 // isolate the demand loop from the prefetch pump

	dest := make([]byte, nBlocks*BlockSize)
	if _, err := m.EnsureAvailableAndRead(ctx, "p", 0, uint32(len(dest)), dest); err != nil {
		t.Fatalf("EnsureAvailableAndRead: %v", err)
	}

	if got := rs.gets.Load(); got != nBlocks {
		t.Fatalf("expected %d remote GETs (one per not-local block), got %d", nBlocks, got)
	}
	if peak := rs.maxInFlight.Load(); peak < 2 {
		t.Fatalf("cold demand fetch ran serially (peak in-flight GETs = %d); "+
			"the serial fetch loop is the cold-read throughput wall", peak)
	}
}
