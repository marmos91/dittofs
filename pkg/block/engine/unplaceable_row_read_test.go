package engine

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	memorylocal "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// manifestBackend names one of the two shapes the covering walk can take: the
// ListFileChunks fallback, and badger's offset-indexed lookups. Neither can lean
// on the other — a payload holding an unplaceable row must refuse on both.
type manifestBackend struct {
	name  string
	build func(t *testing.T) (block.EngineFileChunkStore, metadata.SyncedHashStore)
}

func manifestBackends() []manifestBackend {
	return []manifestBackend{
		{"ListFileChunksFallback", func(t *testing.T) (block.EngineFileChunkStore, metadata.SyncedHashStore) {
			return newStubFileChunkStore(), metadatamemory.NewMemoryMetadataStoreWithDefaults()
		}},
		{"OffsetIndexed", func(t *testing.T) (block.EngineFileChunkStore, metadata.SyncedHashStore) {
			ms, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(context.Background(), t.TempDir())
			if err != nil {
				t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
			}
			// Registered before the engine is built so cleanups run LIFO and the
			// syncer's workers are joined before badger closes under them.
			t.Cleanup(func() { _ = ms.Close() })
			return ms, ms
		}},
	}
}

// newRemoteBackedEngine builds a Store with a real (memory) remote so
// HasRemoteStore reports true and an uncovered read reconciles against the
// manifest.
func newRemoteBackedEngine(t *testing.T, b manifestBackend) (*Store, block.EngineFileChunkStore, *remotememory.Store, metadata.SyncedHashStore) {
	t.Helper()
	fbs, shs := b.build(t)
	localStore := memorylocal.New()
	rs := remotememory.New()

	syncer := NewSyncer(localStore, rs, fbs, DefaultConfig())
	syncer.SetSyncedHashStore(shs)
	syncer.SetRemoteBlockStore(rs)

	bs, err := New(BlockStoreConfig{
		Local:           localStore,
		Remote:          rs,
		Syncer:          syncer,
		FileChunkStore:  fbs,
		SyncedHashStore: shs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs, fbs, rs, shs
}

// TestReadAt_UncoveredOffsetOnUnplaceableRowRefuses pins the composition of the
// covering walk's guard with the cold seeding that skips unplaceable rows.
// Seeding cannot place such a row, so the range it describes gets no cold
// interval and the local tier calls it a plain hole. Classifying the read on the
// cold flag alone would then never reach the guard, and the very rows it was
// written for would be the ones served as zeros for good.
//
// The read must refuse instead: nothing covers the offset and the manifest holds
// a row whose range is unknown, so the zeros would be invented.
func TestReadAt_UncoveredOffsetOnUnplaceableRowRefuses(t *testing.T) {
	for _, b := range manifestBackends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			bs, fbs, _, _ := newRemoteBackedEngine(t, b)
			const payloadID = "payload-unplaceable-read"

			if err := fbs.Put(ctx, &block.FileChunk{ID: payloadID + "/not-an-offset", DataSize: 4096}); err != nil {
				t.Fatalf("seed unplaceable row: %v", err)
			}

			got := bytes.Repeat([]byte{0xAA}, 4096)
			_, err := bs.ReadAt(ctx, payloadID, nil, got, 0)
			if !errors.Is(err, block.ErrManifestInconsistent) {
				t.Fatalf("ReadAt = %v; want ErrManifestInconsistent (served zeros: %v)",
					err, bytes.Equal(got, make([]byte, 4096)))
			}
		})
	}
}

// TestReadAt_CoveredOffsetIgnoresUnplaceableRow keeps the guard scoped to the
// reads it must fail. One bad row makes an unknown range unreadable, not the
// whole payload: an offset some other row covers still answers from that row.
func TestReadAt_CoveredOffsetIgnoresUnplaceableRow(t *testing.T) {
	for _, b := range manifestBackends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			bs, fbs, rs, shs := newRemoteBackedEngine(t, b)
			const payloadID = "payload-partly-unplaceable"

			want := bytes.Repeat([]byte{0x5A}, 4096)
			seedSyncedRemoteChunk(t, fbs, rs, shs, payloadID, 0, want)
			if err := fbs.Put(ctx, &block.FileChunk{ID: payloadID + "/not-an-offset", DataSize: 4096}); err != nil {
				t.Fatalf("seed unplaceable row: %v", err)
			}

			got := make([]byte, len(want))
			if _, err := bs.ReadAt(ctx, payloadID, nil, got, 0); err != nil {
				t.Fatalf("ReadAt over a covered offset: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("covered offset served %x…; want %x…", got[:16], want[:16])
			}
		})
	}
}

// TestReadAt_GenuineHoleStillZeroFills guards the other side: reconciling a hole
// against the manifest must not turn a sparse file into an error. With no row
// covering the window and no unplaceable row to cast doubt, the hole is real.
func TestReadAt_GenuineHoleStillZeroFills(t *testing.T) {
	for _, b := range manifestBackends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			bs, fbs, rs, shs := newRemoteBackedEngine(t, b)
			const payloadID = "payload-genuine-hole"

			// Data at [4096, 8192) only, so [0, 4096) is a real sparse hole.
			seedSyncedRemoteChunk(t, fbs, rs, shs, payloadID, 4096, bytes.Repeat([]byte{0x5A}, 4096))

			got := bytes.Repeat([]byte{0xAA}, 4096)
			n, err := bs.ReadAt(ctx, payloadID, nil, got, 0)
			if err != nil {
				t.Fatalf("ReadAt over a sparse hole: %v", err)
			}
			if n != len(got) {
				t.Fatalf("ReadAt n = %d; want %d", n, len(got))
			}
			if !bytes.Equal(got, make([]byte, 4096)) {
				t.Fatalf("sparse hole not zero-filled")
			}
		})
	}
}

// TestDataExtents_UnplaceableRowReportsWholeFileAsData closes the other op on
// the same seam. SEEK and READ_PLUS derive their hole map from DataExtents and
// never call READ for a range it calls hole, so the covering walk's guard cannot
// protect them: a sparse-copy client would skip the range on SEEK alone and lose
// the bytes. An unplaceable row therefore widens the map to the whole file —
// over-reporting data is the RFC-safe direction, and it forces the READ that
// refuses.
//
// Both callers drop an error from DataExtents and fall back to the CAS block
// list, which cannot see the row either, so refusing here would change nothing.
func TestDataExtents_UnplaceableRowReportsWholeFileAsData(t *testing.T) {
	for _, b := range manifestBackends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			bs, fbs, rs, shs := newRemoteBackedEngine(t, b)
			const payloadID = "payload-extents-unplaceable"
			const fileSize = 16384

			// Real data at [0, 4096) plus a row whose range cannot be placed.
			seedSyncedRemoteChunk(t, fbs, rs, shs, payloadID, 0, bytes.Repeat([]byte{0x5A}, 4096))
			if err := fbs.Put(ctx, &block.FileChunk{ID: payloadID + "/not-an-offset", DataSize: 4096}); err != nil {
				t.Fatalf("seed unplaceable row: %v", err)
			}

			ext, err := bs.DataExtents(ctx, payloadID, fileSize)
			if err != nil {
				t.Fatalf("DataExtents: %v", err)
			}
			want := [][2]uint64{{0, fileSize}}
			if len(ext) != 1 || ext[0] != want[0] {
				t.Fatalf("DataExtents = %v; want %v (a hole here is a range SEEK lets a client skip)", ext, want)
			}
		})
	}
}
