package engine_test

import (
	"bytes"
	"context"
	"math/rand"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// pickInteriorStraddle chooses an overwrite window [d0, d1) over an already
// carved manifest such that the window starts inside one row, covers at least
// one row whole, and ends strictly inside a row that starts inside the window:
// d0 < rowStart < d1 < rowEnd. That is the shape a carve span cannot tile — its
// end is a fresh chunk boundary unrelated to the old row's end — and the row it
// cuts starts inside it, so the row cannot be spared without shadowing the fresh
// tiling and cannot be dropped without stranding its tail.
func pickInteriorStraddle(t *testing.T, refs []block.ChunkRef) (d0, d1 uint64, straddled block.ChunkRef) {
	t.Helper()
	for k := 2; k < len(refs); k++ {
		r := refs[k]
		if r.Size < 8192 || refs[k-2].Size < 8192 {
			continue
		}
		d0 = refs[k-2].Offset + 4096
		d1 = r.Offset + 4096
		return d0, d1, r
	}
	t.Fatalf("manifest has no row shaped for an interior straddle: %d rows", len(refs))
	return 0, 0, block.ChunkRef{}
}

// runInteriorStraddleColdRead drives the reachable sequence end to end on a real
// engine: carve and sync a file, evict it locally, then overwrite a middle range
// that spans more than one old chunk and ends mid-chunk. The old row the span
// cuts starts inside the span, so before the head-narrowing reap it survives
// whole and outranks the fresh rows over [rowStart, spanEnd) — a cold read there
// is served pre-carve bytes.
//
// Three windows are read back, and only one of them is the defect: the head
// straddler (a row starting BEFORE the span) and the untouched cold tail past
// the overwrite are correct on both sides of the fix, so a failure confined to
// the interior straddler is the defect rather than a broken fixture.
func runInteriorStraddleColdRead(t *testing.T, ms metadata.Store) {
	ctx := context.Background()
	bs := newEngineWithRemote(t, ms, remotememory.New())
	root := createShare(t, ms, "straddle")
	pid, _ := createRealFile(t, ms, "straddle", "s.bin", root)

	const fileSize = 8 * 1024 * 1024
	seed := make([]byte, fileSize)
	rand.New(rand.NewSource(0x2128)).Read(seed) //nolint:gosec // deterministic fixture
	if _, err := bs.WriteAt(ctx, pid, nil, seed, 0); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	carve(t, bs, ctx, pid)

	d0, d1, straddled := pickInteriorStraddle(t, manifestRefs(t, ms, pid))

	// Evict before the overwrite: with the file local, carve extends the run to
	// the straddled row's end and the shape never arises. An evicted tail is what
	// the extension cannot reach.
	if _, err := bs.DrainLocalSynced(ctx); err != nil {
		t.Fatalf("evict before overwrite: %v", err)
	}

	want := append([]byte{}, seed...)
	ow := make([]byte, d1-d0)
	rand.New(rand.NewSource(0x2128BEEF)).Read(ow) //nolint:gosec // deterministic fixture
	copy(want[d0:d1], ow)
	if _, err := bs.WriteAt(ctx, pid, nil, ow, d0); err != nil {
		t.Fatalf("overwrite write: %v", err)
	}
	carve(t, bs, ctx, pid)

	// Evict again so every read below has to resolve a manifest row and fetch.
	if _, err := bs.DrainLocalSynced(ctx); err != nil {
		t.Fatalf("evict before read: %v", err)
	}

	read := func(off, end uint64) []byte {
		t.Helper()
		got := make([]byte, end-off)
		if _, err := bs.ReadAt(ctx, pid, got, off); err != nil {
			t.Fatalf("cold read [%d, %d): %v", off, end, err)
		}
		return got
	}
	diff := func(label string, off, end uint64) {
		t.Helper()
		got := read(off, end)
		if bytes.Equal(got, want[off:end]) {
			return
		}
		for i := range got {
			if got[i] != want[off+uint64(i)] {
				at := off + uint64(i)
				t.Errorf("%s: cold read differs at offset %d: got %#x, want %#x (pre-overwrite byte was %#x)",
					label, at, got[i], want[at], seed[at])
				return
			}
		}
	}

	// Control: the row containing d0 starts BEFORE the span and is narrowed off
	// its tail, which has always worked. It must keep working.
	diff("head-straddler", d0, straddled.Offset)
	// The defect: the row starting at straddled.Offset also reaches past d1.
	diff("interior-straddler", straddled.Offset, d1)
	// Control: bytes past the span were never re-carved and must still read back
	// as they were seeded.
	diff("untouched-tail", d1, min(straddled.Offset+uint64(straddled.Size)+(1<<20), fileSize))
}

func TestMemoryColdRead_InteriorStraddler(t *testing.T) {
	runInteriorStraddleColdRead(t, metadatamemory.NewMemoryMetadataStoreWithDefaults())
}

func TestBadgerColdRead_InteriorStraddler(t *testing.T) {
	ms, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	runInteriorStraddleColdRead(t, ms)
}
