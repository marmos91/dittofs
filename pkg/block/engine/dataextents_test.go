package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// TestStore_DataExtents_UnionsLocalAndCAS verifies the #1481 fix at the engine
// level: DataExtents must union the pre-rollup local append-log view with the
// persisted CAS FileChunk manifest, so SEEK / READ_PLUS see the same data the
// READ path reconstructs across all tiers. A region present only in the local
// append log (not yet rolled up) and a region present only in CAS must BOTH
// surface as data, while the gap between them stays a hole.
func TestStore_DataExtents_UnionsLocalAndCAS(t *testing.T) {
	ctx := context.Background()
	coord := newRefcountCoordinator()
	fbs := newStubFileChunkStore()
	bs := newReapFixture(t, coord, fbs)

	const payloadID = "union"
	const casOff = uint64(1 << 20)
	const chunk = uint64(4096)
	fileSize := casOff + chunk

	// Pre-carve bytes: live only in the local journal tier at [0, 4096).
	if err := bs.local.WriteAt(ctx, payloadID, 0, make([]byte, chunk)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	// Post-rollup bytes: a committed CAS chunk at [1 MiB, 1 MiB+4096) that the
	// local append log does NOT cover.
	if err := fbs.Put(ctx, &block.FileChunk{
		ID:       fmt.Sprintf("%s/%d", payloadID, casOff),
		Hash:     hashN(0x7E),
		DataSize: uint32(chunk),
	}); err != nil {
		t.Fatalf("seed CAS chunk: %v", err)
	}

	ext, err := bs.DataExtents(ctx, payloadID, fileSize)
	if err != nil {
		t.Fatalf("DataExtents: %v", err)
	}

	covered := func(off uint64) bool {
		for _, e := range ext {
			if off >= e[0] && off < e[1] {
				return true
			}
		}
		return false
	}
	// Local (pre-rollup) region.
	if !covered(0) || !covered(chunk-1) {
		t.Errorf("local append-log region [0,%d) not covered: %v", chunk, ext)
	}
	// CAS-only (post-rollup) region.
	if !covered(casOff) || !covered(casOff+chunk-1) {
		t.Errorf("CAS region [%d,%d) not covered: %v", casOff, casOff+chunk, ext)
	}
	// The gap between the two regions must remain a hole.
	if covered(casOff / 2) {
		t.Errorf("interior gap byte %d reported as data (false-data): %v", casOff/2, ext)
	}
	// Never report data past EOF.
	for i, e := range ext {
		if e[1] > fileSize {
			t.Errorf("extent %d = [%d,%d) exceeds fileSize %d", i, e[0], e[1], fileSize)
		}
	}
}
