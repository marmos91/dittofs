package engine_test

import (
	"context"
	"math/rand"
	"testing"

	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

func TestColdReadAtCarveSeam_ShadowedByStaleStraddler(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs := newEngineWithRemote(t, ms, remotememory.New())
	root := createShare(t, ms, "seam")
	pid, _ := createRealFile(t, ms, "seam", "s.bin", root)

	rng := rand.New(rand.NewSource(0x50AC + 22)) //nolint:gosec
	seed := make([]byte, 6*1024*1024)
	rng.Read(seed)
	if _, err := bs.WriteAt(ctx, pid, nil, seed, 0); err != nil {
		t.Fatal(err)
	}
	carve(t, bs, ctx, pid)

	// Split the punch at a 2 MiB seam, carving in between — what a background
	// carve firing mid-punch produces.
	const off, length = 2092552, 3316792
	if _, err := bs.PunchHole(ctx, pid, manifestRefs(t, ms, pid), off, 2<<20); err != nil {
		t.Fatal(err)
	}
	carve(t, bs, ctx, pid)
	t.Log("--- after punch-1 carve ---")
	for _, r := range manifestRefs(t, ms, pid) {
		t.Logf("row off=%d size=%d end=%d hash=%.10s", r.Offset, r.Size, r.Offset+uint64(r.Size), r.Hash)
	}
	if _, err := bs.PunchHole(ctx, pid, manifestRefs(t, ms, pid), off+(2<<20), length-(2<<20)); err != nil {
		t.Fatal(err)
	}
	carve(t, bs, ctx, pid)

	for _, r := range manifestRefs(t, ms, pid) {
		t.Logf("row off=%d size=%d end=%d hash=%.10s", r.Offset, r.Size, r.Offset+uint64(r.Size), r.Hash)
	}
	warm := make([]byte, 32)
	if _, err := bs.ReadAt(ctx, pid, nil, warm, 4189704); err != nil {
		t.Fatal(err)
	}
	t.Logf("WARM[4189704:32] = %v", warm[:8])
	if _, err := bs.DrainLocalSynced(ctx); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, length)
	if _, err := bs.ReadAt(ctx, pid, nil, got, off); err != nil {
		t.Fatal(err)
	}
	bad := 0
	for i, b := range got {
		if b != 0 {
			if bad < 6 {
				t.Logf("nonzero at abs %d = %d (seed had %d)", off+uint64(i), b, seed[off+uint64(i)])
			}
			bad++
		}
	}
	if bad > 0 {
		t.Fatalf("%d nonzero bytes in punched range", bad)
	}
}
