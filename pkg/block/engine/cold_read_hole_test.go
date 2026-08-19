package engine

import (
	"bytes"
	"context"
	"errors"
	"math"
	"math/rand"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	memorylocal "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// indexedChunkStore gives stubFileChunkStore the two offset-indexed lookups the
// badger backend implements, so resolveCovering and resolveNextChunkStart take
// their indexed branches instead of the ListFileChunks fallback. Both answers
// are derived from the same rows the fallback would walk, so the two store
// shapes must agree.
type indexedChunkStore struct{ *stubFileChunkStore }

func (s *indexedChunkStore) GetFileChunkAtOffset(ctx context.Context, payloadID string, off uint64) (*block.FileChunk, error) {
	rows, err := s.ListFileChunks(ctx, payloadID)
	if err != nil {
		return nil, err
	}
	rw, err := findRowCoveringOffset(rows, off)
	if err != nil || rw == nil {
		return nil, err
	}
	return rw.fb, nil
}

func (s *indexedChunkStore) GetFileChunkAtOrAfterOffset(ctx context.Context, payloadID string, off uint64) (*block.FileChunk, error) {
	rows, err := s.ListFileChunks(ctx, payloadID)
	if err != nil {
		return nil, err
	}
	var (
		best    *block.FileChunk
		bestOff uint64
	)
	for _, fb := range rows {
		abs, ok := block.ParseChunkOffset(fb.ID)
		if !ok || abs < off {
			continue
		}
		if best == nil || abs < bestOff {
			best, bestOff = fb, abs
		}
	}
	return best, nil
}

// TestEnsureAvailableAndRead_ChunkAfterHoleIsFetched pins the sparse-hole step
// in the per-window fetch loop. The payload is a hole over [0, 1 MiB) with a
// real remote-resident chunk at [1 MiB, 3 MiB) — both inside the first 8 MiB
// block, which is the routine shape because FastCDC chunks average well under
// BlockSize.
//
// Advancing to the next block boundary on the uncovered first byte discards
// every chunk up to that boundary, so the chunk after the hole is never
// dispatched and never hydrated; the caller's post-fetch re-read then serves it
// as zeros. The loop must instead resume at the next chunk start, so the read
// spanning [0, 4 MiB) leaves the chunk warm in the local tier.
//
// Both store shapes are exercised because they resolve the successor by
// different means — an offset index versus a manifest walk.
func TestEnsureAvailableAndRead_ChunkAfterHoleIsFetched(t *testing.T) {
	const (
		oneMiB     = 1024 * 1024
		holeEnd    = 1 * oneMiB
		chunkSize  = 2 * oneMiB
		readLength = 4 * oneMiB
	)

	for _, tc := range []struct {
		name string
		wrap func(*stubFileChunkStore) block.EngineFileChunkStore
	}{
		{"ListFileChunksFallback", func(s *stubFileChunkStore) block.EngineFileChunkStore { return s }},
		{"OffsetIndexed", func(s *stubFileChunkStore) block.EngineFileChunkStore { return &indexedChunkStore{s} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			payloadID := "payload-hole-then-chunk"

			chunk := make([]byte, chunkSize)
			rand.New(rand.NewSource(0xC01D)).Read(chunk) //nolint:gosec // deterministic fixture

			loc := memorylocal.New()
			rs := remotememory.New()
			stub := newStubFileChunkStore()
			mds := metadatamemory.NewMemoryMetadataStoreWithDefaults()

			// Only the post-hole chunk is seeded, so [0, 1 MiB) has no covering row.
			seedSyncedRemoteChunk(t, stub, rs, mds, payloadID, holeEnd, chunk)

			m := newFetchSyncer(loc, rs, tc.wrap(stub), mds)

			dest := make([]byte, readLength)
			if err := m.EnsureAvailableAndRead(ctx, payloadID, 0, readLength, dest); err != nil {
				t.Fatalf("EnsureAvailableAndRead: %v", err)
			}

			// Poison the destination so an unhydrated range fails instead of hiding in zeros.
			got := bytes.Repeat([]byte{0xAA}, chunkSize)
			n, st, err := loc.ReadAt(ctx, payloadID, holeEnd, got)
			if err != nil {
				t.Fatalf("local ReadAt after fetch: %v", err)
			}
			if st.Cold {
				t.Fatalf("local ReadAt reports cold after fetch; chunk at %d was never hydrated", holeEnd)
			}
			if n != chunkSize {
				t.Fatalf("local ReadAt n = %d; want %d", n, chunkSize)
			}
			if !bytes.Equal(got, chunk) {
				t.Fatalf("chunk after hole not hydrated: got %x…, want %x…", got[:16], chunk[:16])
			}
		})
	}
}

// successorStub returns a fixed row from the indexed successor lookup, standing
// in for a backend whose offset index still points at a row whose ID cannot be
// placed.
type successorStub struct {
	*stubFileChunkStore
	row *block.FileChunk
}

func (s *successorStub) GetFileChunkAtOrAfterOffset(_ context.Context, _ string, _ uint64) (*block.FileChunk, error) {
	return s.row, nil
}

// TestResolveNextChunkStart_UnplaceableRowIsReported pins the failure mode of a
// row whose ID carries no offset. Such a row sits at an unknown position, so it
// may be the very next chunk after the hole being stepped over. Skipping it and
// returning a later start would hand the caller a wider hole than the file has
// and the bytes it holds would be zero-filled, so the manifest inconsistency is
// reported instead.
//
// Both lookup branches are covered because neither can lean on the other having
// run: a backend may index coverage without indexing succession, and the walks
// are separate snapshots.
func TestResolveNextChunkStart_UnplaceableRowIsReported(t *testing.T) {
	ctx := context.Background()
	const payloadID = "payload-unplaceable"
	bad := &block.FileChunk{ID: payloadID + "/not-an-offset", DataSize: 4096}

	t.Run("ListFileChunksFallback", func(t *testing.T) {
		stub := newStubFileChunkStore()
		if err := stub.Put(ctx, bad); err != nil {
			t.Fatalf("seed unplaceable row: %v", err)
		}
		// A placeable row further out must not become the answer.
		if err := stub.Put(ctx, &block.FileChunk{ID: payloadID + "/8192", DataSize: 4096}); err != nil {
			t.Fatalf("seed placeable row: %v", err)
		}
		_, _, err := resolveNextChunkStart(ctx, stub, payloadID, 0)
		if !errors.Is(err, block.ErrManifestInconsistent) {
			t.Fatalf("resolveNextChunkStart = %v; want ErrManifestInconsistent", err)
		}
	})

	t.Run("OffsetIndexed", func(t *testing.T) {
		store := &successorStub{stubFileChunkStore: newStubFileChunkStore(), row: bad}
		_, _, err := resolveNextChunkStart(ctx, store, payloadID, 0)
		if !errors.Is(err, block.ErrManifestInconsistent) {
			t.Fatalf("resolveNextChunkStart = %v; want ErrManifestInconsistent", err)
		}
	})
}

// TestResolveNextChunkStart_MaxOffsetDoesNotWrap pins the probe-offset guard: at
// the last representable offset there is no "strictly after", and computing
// off+1 would wrap to zero and restart the search at the front of the payload.
func TestResolveNextChunkStart_MaxOffsetDoesNotWrap(t *testing.T) {
	ctx := context.Background()
	const payloadID = "payload-max-offset"
	store := &successorStub{
		stubFileChunkStore: newStubFileChunkStore(),
		row:                &block.FileChunk{ID: payloadID + "/0", DataSize: 4096},
	}
	got, ok, err := resolveNextChunkStart(ctx, store, payloadID, math.MaxUint64)
	if err != nil {
		t.Fatalf("resolveNextChunkStart: %v", err)
	}
	if ok {
		t.Fatalf("resolveNextChunkStart(MaxUint64) = (%d, true); want no successor", got)
	}
}
