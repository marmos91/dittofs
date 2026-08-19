package engine

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/local"
	memorylocal "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestFetchBlock_StagesEveryChunkInBlock pins the readahead prefetch to the
// whole block it was asked for. A block spans BlockSize while FastCDC chunks
// average well under that, so a block routinely holds several chunks; here two
// 2 MiB chunks sit at offsets 0 and 2 MiB, both inside block 0.
//
// Resolving a single row at the block-aligned offset finds only the first chunk
// and leaves the rest of the block cold, which quietly turns a block of
// lookahead into a chunk of lookahead: the demand read that follows pays the
// remote round-trips the prefetch was supposed to have already hidden. Both
// chunks must be staged.
//
// Both store shapes are exercised because they resolve coverage by different
// means — an offset index versus a manifest walk.
func TestFetchBlock_StagesEveryChunkInBlock(t *testing.T) {
	const (
		oneMiB    = 1024 * 1024
		chunkSize = 2 * oneMiB
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
			payloadID := "payload-prefetch-multi-chunk"

			first := make([]byte, chunkSize)
			second := make([]byte, chunkSize)
			rng := rand.New(rand.NewSource(0xFEED)) //nolint:gosec // deterministic fixture
			rng.Read(first)
			rng.Read(second)

			loc := memorylocal.New()
			rs := remotememory.New()
			stub := newStubFileChunkStore()
			mds := metadatamemory.NewMemoryMetadataStoreWithDefaults()

			seedSyncedRemoteChunk(t, stub, rs, mds, payloadID, 0, first)
			seedSyncedRemoteChunk(t, stub, rs, mds, payloadID, chunkSize, second)

			m := newFetchSyncer(loc, rs, tc.wrap(stub), mds)

			if err := m.fetchBlock(ctx, payloadID, 0); err != nil {
				t.Fatalf("fetchBlock: %v", err)
			}

			for _, want := range []struct {
				offset int64
				data   []byte
			}{{0, first}, {chunkSize, second}} {
				// Poison the destination so an unhydrated range fails loudly
				// instead of matching a zero-filled read.
				got := bytes.Repeat([]byte{0xAA}, chunkSize)
				n, st, err := loc.ReadAt(ctx, payloadID, want.offset, got)
				if err != nil {
					t.Fatalf("local ReadAt at %d: %v", want.offset, err)
				}
				if st.Cold {
					t.Fatalf("chunk at %d still cold: prefetch skipped it", want.offset)
				}
				if n != chunkSize {
					t.Fatalf("local ReadAt at %d n = %d; want %d", want.offset, n, chunkSize)
				}
				if !bytes.Equal(got, want.data) {
					t.Fatalf("chunk at %d not staged: got %x…, want %x…", want.offset, got[:16], want.data[:16])
				}
			}
		})
	}
}

// alwaysColdLocal reports every read as cold, standing in for a window whose
// bytes the hydrate could not bring back — an evicted range whose manifest row
// no longer resolves, or an eviction racing the hydrate. Only ReadAt is
// exercised, so the embedded interface stays nil.
type alwaysColdLocal struct {
	local.LocalStore
}

func (alwaysColdLocal) ReadAt(_ context.Context, _ string, _ int64, dst []byte) (int, journal.ReadState, error) {
	return len(dst), journal.ReadState{Cold: true}, nil
}

// TestReadAtInternal_StillColdAfterHydrateFailsClosed pins the post-hydrate
// re-read to its own cold flag. Hydration is meant to make every
// written-but-evicted byte in the window local again, so a re-read still
// reporting cold means bytes the manifest claims were written could not be
// brought back.
//
// journal.ReadAt zero-fills whatever it cannot serve, so ignoring that flag
// returns a full-length success made of zeros — the silent-hole failure mode,
// with no error at any layer. It must surface as ErrChunkNotFound instead.
//
// A never-written hole does not report cold, so a genuinely sparse file is
// unaffected; a zero-length read returns before the hydrate path and cannot
// reach the guard at all.
func TestReadAtInternal_StillColdAfterHydrateFailsClosed(t *testing.T) {
	ctx := context.Background()

	// No remote: EnsureAvailable returns without hydrating anything, so
	// the window is still cold on the re-read.
	bs := &Store{
		local:  alwaysColdLocal{},
		syncer: &Syncer{stopCh: make(chan struct{}), config: DefaultConfig()},
	}

	data := bytes.Repeat([]byte{0xAA}, 4096)
	n, err := bs.readAtInternal(ctx, "payload-still-cold", data, 0)
	if err == nil {
		t.Fatalf("readAtInternal returned (%d, nil) for a window still cold after hydrate; want an error", n)
	}
	if !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("err = %v; want errors.Is(block.ErrChunkNotFound)", err)
	}

	// A zero-length read has no window to be cold about and must stay a
	// no-error no-op rather than tripping the new guard.
	if n, err := bs.readAtInternal(ctx, "payload-still-cold", nil, 0); err != nil || n != 0 {
		t.Fatalf("readAtInternal(empty) = (%d, %v); want (0, nil)", n, err)
	}
}
