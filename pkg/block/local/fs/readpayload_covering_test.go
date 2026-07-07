package fs

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newCoveringTestStore wires an FSStore whose FileChunkStore (memFBS)
// implements GetFileChunkAtOffset, so post-rollup reads drive
// fillFromCASManifest's indexed fast path.
func newCoveringTestStore(t *testing.T) (*FSStore, context.Context) {
	t.Helper()
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	fbs := newMemFileChunkStore()
	persister := func(ctx context.Context, payloadID string, blocks []block.ChunkRef, _ block.ObjectID) error {
		return fbs.persist(ctx, payloadID, blocks)
	}
	bc := newFSStoreForTestWithFBS(t, fbs, FSStoreOptions{
		MaxLogBytes:       1 << 30,
		RollupWorkers:     2,
		StabilizationMS:   3_600_000,
		RollupStore:       rs,
		ObjectIDPersister: persister,
	})
	return bc, context.Background()
}

// TestReadPayloadAt_CoveringFastPath_MultiChunk writes a multi-chunk file,
// rolls it into CAS, then reads several sub-windows (chunk-aligned,
// mid-chunk, and spanning chunk boundaries). Every read must return the
// exact source bytes — this exercises the fast path's `cur = chunkEnd`
// advance across more than one covering chunk.
func TestReadPayloadAt_CoveringFastPath_MultiChunk(t *testing.T) {
	bc, ctx := newCoveringTestStore(t)

	// Position-dependent bytes so any mis-offset copy is caught.
	const size = 1 << 20 // 1 MiB — large enough for several FastCDC chunks
	src := make([]byte, size)
	for i := range src {
		src[i] = byte(i*31 + 7)
	}
	if err := bc.AppendWrite(ctx, "fileM", src, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	if err := bc.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups: %v", err)
	}

	windows := []struct {
		name        string
		off, length uint64
	}{
		{"head", 0, 4096},
		{"mid-unaligned", 4097, 4096},
		{"large-span", 300000, 200000},
		{"tail", size - 4096, 4096},
		{"whole", 0, size},
	}
	for _, w := range windows {
		got := make([]byte, w.length)
		if _, err := bc.ReadPayloadAt(ctx, "fileM", got, w.off); err != nil {
			t.Fatalf("%s: ReadPayloadAt(off=%d len=%d): %v", w.name, w.off, w.length, err)
		}
		if !bytes.Equal(got, src[w.off:w.off+w.length]) {
			t.Fatalf("%s: read bytes mismatch at off=%d", w.name, w.off)
		}
	}
}

// TestReadPayloadAt_CoveringFastPath_Hole writes two separated regions,
// leaving an un-backed gap between them. A read spanning the gap must
// surface a miss (ErrFileChunkNotFound) — the fast path detects the hole
// and hands off to the scan fallback, which also leaves it uncovered.
// Reads confined to each backed region must still succeed.
func TestReadPayloadAt_CoveringFastPath_Hole(t *testing.T) {
	bc, ctx := newCoveringTestStore(t)

	a := bytes.Repeat([]byte{'A'}, 4096)
	b := bytes.Repeat([]byte{'B'}, 4096)
	if err := bc.AppendWrite(ctx, "fileH", a, 0); err != nil {
		t.Fatalf("AppendWrite region A: %v", err)
	}
	// Leave [4096, 8192) a hole; write region B at 8192.
	if err := bc.AppendWrite(ctx, "fileH", b, 8192); err != nil {
		t.Fatalf("AppendWrite region B: %v", err)
	}
	if err := bc.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups: %v", err)
	}

	// Each backed region reads back cleanly.
	got := make([]byte, 4096)
	if _, err := bc.ReadPayloadAt(ctx, "fileH", got, 0); err != nil || !bytes.Equal(got, a) {
		t.Fatalf("region A read: err=%v equal=%v", err, bytes.Equal(got, a))
	}
	if _, err := bc.ReadPayloadAt(ctx, "fileH", got, 8192); err != nil || !bytes.Equal(got, b) {
		t.Fatalf("region B read: err=%v equal=%v", err, bytes.Equal(got, b))
	}

	// A read that spans the hole is not fully local → miss.
	span := make([]byte, 12288)
	if _, err := bc.ReadPayloadAt(ctx, "fileH", span, 0); !errors.Is(err, block.ErrFileChunkNotFound) {
		t.Fatalf("hole-spanning read: got err=%v, want ErrFileChunkNotFound", err)
	}
}
