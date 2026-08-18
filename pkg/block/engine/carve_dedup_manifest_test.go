package engine_test

import (
	"context"
	"math/rand"
	"testing"

	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestCarveDedupedChunk_KeepsManifestRow: a carve run whose chunks all dedup
// against already-durable hashes must still write their manifest rows. Without
// them the run-end reap (which treats every carved offset as covered) deletes
// the rows that used to back the range, and the cold read resolves a stale
// straddler or a gap.
func TestCarveDedupedChunk_KeepsManifestRow(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs := newEngineWithRemote(t, ms, remotememory.New())
	root := createShare(t, ms, "dedup")
	pid, _ := createRealFile(t, ms, "dedup", "dup.bin", root)

	const half = 4 * 1024 * 1024
	data := make([]byte, half)
	rand.New(rand.NewSource(0xD00D)).Read(data) //nolint:gosec // deterministic fixture

	if _, err := bs.WriteAt(ctx, pid, nil, data, 0); err != nil {
		t.Fatalf("first WriteAt: %v", err)
	}
	carve(t, bs, ctx, pid)

	// The same bytes at a fresh offset: every chunk hashes to an already-durable
	// chunk, so the whole run dedups.
	if _, err := bs.WriteAt(ctx, pid, nil, data, half); err != nil {
		t.Fatalf("duplicate WriteAt: %v", err)
	}
	carve(t, bs, ctx, pid)

	if _, err := bs.DrainLocalSynced(ctx); err != nil {
		t.Fatalf("DrainLocalSynced: %v", err)
	}
	got := make([]byte, half)
	if _, err := bs.ReadAt(ctx, pid, nil, got, half); err != nil {
		t.Fatalf("cold ReadAt: %v", err)
	}
	if i := firstDiff(data, got); i >= 0 {
		t.Fatalf("cold read of the deduped copy differs at byte %d (abs %d): got %d want %d",
			i, half+i, got[i], data[i])
	}
}

// TestPunchHole_RepunchReadsBackZerosCold: re-punching a range re-carves zeros
// that already dedup, so the run packs no novel chunk. The range must still read
// back as zeros after eviction (RFC 7862 DEALLOCATE).
func TestPunchHole_RepunchReadsBackZerosCold(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs := newEngineWithRemote(t, ms, remotememory.New())
	root := createShare(t, ms, "punch2")
	pid, _ := createRealFile(t, ms, "punch2", "p.bin", root)

	const size = 8 * 1024 * 1024
	data := make([]byte, size)
	rand.New(rand.NewSource(0xF00D)).Read(data) //nolint:gosec // deterministic fixture
	if _, err := bs.WriteAt(ctx, pid, nil, data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	carve(t, bs, ctx, pid)

	punch := func(off, length uint64) {
		t.Helper()
		if _, err := bs.PunchHole(ctx, pid, manifestRefs(t, ms, pid), off, length); err != nil {
			t.Fatalf("PunchHole(%d,%d): %v", off, length, err)
		}
		carve(t, bs, ctx, pid)
		clear(data[off : off+length])
	}
	// Re-punching the same range is idempotent per RFC 7862; the second pass
	// re-carves the identical zeros, so every chunk dedups and the run packs no
	// novel chunk at all.
	punch(1<<20, 2<<20)
	punch(1<<20, 2<<20)

	if _, err := bs.DrainLocalSynced(ctx); err != nil {
		t.Fatalf("DrainLocalSynced: %v", err)
	}
	got := make([]byte, size)
	if _, err := bs.ReadAt(ctx, pid, nil, got, 0); err != nil {
		t.Fatalf("cold ReadAt: %v", err)
	}
	if i := firstDiff(data, got); i >= 0 {
		t.Fatalf("cold read mismatch at byte %d: got %d want %d", i, got[i], data[i])
	}
}

// firstDiff returns the index of the first byte where want and got differ, or
// -1 when they are identical. Both must be the same length.
func firstDiff(want, got []byte) int {
	for i := range want {
		if want[i] != got[i] {
			return i
		}
	}
	return -1
}
