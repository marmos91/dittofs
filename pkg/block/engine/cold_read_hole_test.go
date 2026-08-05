package engine

import (
	"bytes"
	"context"
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
			if _, err := m.EnsureAvailableAndRead(ctx, payloadID, 0, readLength, dest); err != nil {
				t.Fatalf("EnsureAvailableAndRead: %v", err)
			}

			// Poison the destination so an unhydrated range fails instead of hiding in zeros.
			got := bytes.Repeat([]byte{0xAA}, chunkSize)
			n, cold, err := loc.ReadAt(ctx, payloadID, holeEnd, got)
			if err != nil {
				t.Fatalf("local ReadAt after fetch: %v", err)
			}
			if cold {
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
