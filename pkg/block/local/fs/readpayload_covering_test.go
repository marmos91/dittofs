package fs

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// memFBS must satisfy the fast-path resolver so ReadPayloadAt drives the
// indexed covering + successor loop rather than the ListFileChunks scan.
var _ coveringChunkResolver = (*memFBS)(nil)

// newCoveringTestStore wires an FSStore whose FileChunkStore (memFBS)
// implements the covering + successor resolver, so post-rollup reads drive
// fillFromCASManifest's indexed fast path.
func newCoveringTestStore(t *testing.T) (*FSStore, context.Context) {
	t.Helper()
	fbs := newRollupMemFileChunkStore()
	persister := func(ctx context.Context, payloadID string, blocks []block.ChunkRef, _ block.ObjectID) error {
		return fbs.persist(ctx, payloadID, blocks)
	}
	bc := newFSStoreForTestWithFBS(t, fbs, FSStoreOptions{
		MaxLogBytes:       1 << 30,
		RollupWorkers:     2,
		StabilizationMS:   3_600_000,
		ObjectIDPersister: persister,
	})
	return bc, context.Background()
}

// TestReadPayloadAt_CoveringFastPath_MultiChunk writes a multi-chunk file,
// rolls it into CAS, then reads several sub-windows (chunk-aligned, mid-chunk,
// and spanning chunk boundaries). Every read must return the exact source bytes
// — this exercises the fast path's `cur = chunkEnd` advance across more than one
// covering chunk.
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

// TestReadPayloadAt_CoveringFastPath_Hole writes two separated regions, leaving
// an un-backed gap between them, then verifies the successor lookup jumps the
// hole without ever serving a neighbour chunk's bytes into it (the silent-
// corruption guard). Each backed region reads back cleanly; any read that
// touches the hole surfaces a miss (ErrFileChunkNotFound) rather than wrong
// bytes.
func TestReadPayloadAt_CoveringFastPath_Hole(t *testing.T) {
	bc, ctx := newCoveringTestStore(t)

	a := bytes.Repeat([]byte{'A'}, 4096)
	c := bytes.Repeat([]byte{'C'}, 4096)
	if err := bc.AppendWrite(ctx, "fileH", a, 0); err != nil {
		t.Fatalf("AppendWrite region A: %v", err)
	}
	// Leave [4096, 8192) a hole; write region C at 8192.
	if err := bc.AppendWrite(ctx, "fileH", c, 8192); err != nil {
		t.Fatalf("AppendWrite region C: %v", err)
	}
	if err := bc.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups: %v", err)
	}

	// Each backed region reads back cleanly — proves the successor jump lands
	// on the right chunk (C is not offset into A's slot or vice versa).
	got := make([]byte, 4096)
	if _, err := bc.ReadPayloadAt(ctx, "fileH", got, 0); err != nil || !bytes.Equal(got, a) {
		t.Fatalf("region A read: err=%v equal=%v", err, bytes.Equal(got, a))
	}
	if _, err := bc.ReadPayloadAt(ctx, "fileH", got, 8192); err != nil || !bytes.Equal(got, c) {
		t.Fatalf("region C read: err=%v equal=%v", err, bytes.Equal(got, c))
	}

	// A read confined to the hole must miss, not serve A's or C's bytes. If the
	// covering guard were absent, GetFileChunkAtOffset could return a neighbour
	// chunk and this would silently succeed with wrong data.
	hole := make([]byte, 4096)
	if _, err := bc.ReadPayloadAt(ctx, "fileH", hole, 4096); !errors.Is(err, block.ErrFileChunkNotFound) {
		t.Fatalf("hole read: got err=%v, want ErrFileChunkNotFound", err)
	}

	// A read spanning the hole is not fully local → miss.
	span := make([]byte, 12288)
	if _, err := bc.ReadPayloadAt(ctx, "fileH", span, 0); !errors.Is(err, block.ErrFileChunkNotFound) {
		t.Fatalf("hole-spanning read: got err=%v, want ErrFileChunkNotFound", err)
	}
}
