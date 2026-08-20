package engine_test

import (
	"context"
	"math/rand"
	"testing"

	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestColdReadAtCarveSeam_PunchedRangeStaysZero drives the overlapping-row
// hazard through the real carve path instead of a hand-built manifest.
//
// Two punches split at a seam, with a carve in between, is what a background
// carve firing mid-punch produces. The second run's fresh tiling starts at an
// offset an older row already straddles, and the run-end reap only deletes rows
// whose START lies inside the run, so that older row survives and overlaps the
// fresh one. Warm the overlap reads correctly — coverage resolves it to the
// greatest start — but a cold read has to fetch both rows, and hydrating the
// straddler's full extent puts the pre-punch bytes back over the fresh row's
// head. The punched range must read as zeros from either tier.
func TestColdReadAtCarveSeam_PunchedRangeStaysZero(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs := newEngineWithRemote(t, ms, remotememory.New())
	root := createShare(t, ms, "seam")
	pid, _ := createRealFile(t, ms, "seam", "s.bin", root)

	rng := rand.New(rand.NewSource(0x50AC + 22)) //nolint:gosec // deterministic fixture
	seed := make([]byte, 6*1024*1024)
	rng.Read(seed)
	if _, err := bs.WriteAt(ctx, pid, nil, seed, 0); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	carve(t, bs, ctx, pid)

	// The offsets are chosen so the first punch's carve leaves a row straddling
	// the second punch's run start.
	const off, length = 2092552, 3316792
	if _, err := bs.PunchHole(ctx, pid, manifestRefs(t, ms, pid), off, 2<<20); err != nil {
		t.Fatalf("first punch: %v", err)
	}
	carve(t, bs, ctx, pid)
	if _, err := bs.PunchHole(ctx, pid, manifestRefs(t, ms, pid), off+(2<<20), length-(2<<20)); err != nil {
		t.Fatalf("second punch: %v", err)
	}
	carve(t, bs, ctx, pid)

	// The run-end reap narrows the row the second run's tiling starts inside, so
	// the manifest is a clean tiling again rather than an overlap the read path
	// has to defend against.
	assertManifestTiles(t, ms, pid, int64(len(seed)), "after-second-carve")

	// Evict the local copy so the read has to come back from the remote store
	// through the manifest.
	if _, err := bs.DrainLocalSynced(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	got := make([]byte, length)
	if _, err := bs.ReadAt(ctx, pid, got, off); err != nil {
		t.Fatalf("cold read: %v", err)
	}
	for i, b := range got {
		if b != 0 {
			for _, r := range manifestRefs(t, ms, pid) {
				t.Logf("row off=%d size=%d end=%d", r.Offset, r.Size, r.Offset+uint64(r.Size))
			}
			t.Fatalf("cold read of punched range returned data at offset %d: got %#x, pre-punch byte was %#x",
				off+uint64(i), b, seed[off+uint64(i)])
		}
	}
}

// TestColdReadAtCarveSeam_RunEndInsideRowStaysCovered is the mirror of the test
// above: instead of a run that STARTS inside an older row, one that ENDS inside
// it. The reap deletes that row — its start lies in the run — and the part past
// the run end has no other cover, because a row claims a prefix of its chunk and
// so cannot start mid-chunk. Carve has to reach the row's end for the manifest
// to keep tiling, and the whole file has to read back byte-for-byte from the
// remote tier afterwards.
//
// The shape is reached by a second punch whose END lands on the journal interval
// boundary the first punch left inside a manifest row: nothing partially overlaps
// a warm interval there, so the run stops exactly at the boundary, mid-row.
func TestColdReadAtCarveSeam_RunEndInsideRowStaysCovered(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs := newEngineWithRemote(t, ms, remotememory.New())
	root := createShare(t, ms, "runend")
	pid, _ := createRealFile(t, ms, "runend", "r.bin", root)

	rng := rand.New(rand.NewSource(0x50AC + 22)) //nolint:gosec // deterministic fixture
	seed := make([]byte, 6*1024*1024)
	rng.Read(seed)
	if _, err := bs.WriteAt(ctx, pid, nil, seed, 0); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	carve(t, bs, ctx, pid)

	const firstOff, firstLen = 2092552, 2 << 20 // ends at 4189704
	if _, err := bs.PunchHole(ctx, pid, manifestRefs(t, ms, pid), firstOff, firstLen); err != nil {
		t.Fatalf("first punch: %v", err)
	}
	carve(t, bs, ctx, pid)

	const secondOff = 1_000_000
	if _, err := bs.PunchHole(ctx, pid, manifestRefs(t, ms, pid), secondOff, firstOff+firstLen-secondOff); err != nil {
		t.Fatalf("second punch: %v", err)
	}
	carve(t, bs, ctx, pid)

	assertManifestTiles(t, ms, pid, int64(len(seed)), "after-second-carve")

	if _, err := bs.DrainLocalSynced(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	want := append([]byte{}, seed...)
	for i := secondOff; i < firstOff+firstLen; i++ {
		want[i] = 0
	}
	got := make([]byte, len(seed))
	if _, err := bs.ReadAt(ctx, pid, got, 0); err != nil {
		t.Fatalf("cold read: %v", err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cold read differs at offset %d: got %#x, want %#x", i, got[i], want[i])
		}
	}
}
