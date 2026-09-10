package shares

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/block/local"
	"github.com/marmos91/dittofs/pkg/block/local/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	badgerstore "github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// buildReconcileFixture creates n regular files nested four directories deep,
// each with a payload the local store also holds, so the scan sees the shape a
// warmed share presents at start: every locally-resident file matched by a
// metadata row whose size is already correct.
func buildReconcileFixture(tb testing.TB, n int) (*badgerstore.BadgerMetadataStore, *memory.MemoryStore, []string) {
	tb.Helper()
	ctx := context.Background()

	store, err := badgerstore.NewBadgerMetadataStoreWithDefaults(ctx, tb.TempDir())
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = store.Close() })

	const share = "reconcile"
	rootFile, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755})
	require.NoError(tb, err)
	root, err := metadata.EncodeFileHandle(rootFile)
	require.NoError(tb, err)

	mkdir := func(parent metadata.FileHandle, path, name string) metadata.FileHandle {
		h, err := store.GenerateHandle(ctx, share, path)
		require.NoError(tb, err)
		_, id, err := metadata.DecodeFileHandle(h)
		require.NoError(tb, err)
		f := &metadata.File{ShareName: share, Path: path, FileAttr: metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755}}
		f.ID = id
		require.NoError(tb, store.UpdateAttrs(ctx, f))
		require.NoError(tb, store.SetParent(ctx, h, parent))
		require.NoError(tb, store.SetChild(ctx, parent, name, h))
		return h
	}

	local := memory.New()
	ids := make([]string, n)
	for i := range ids {
		dir, path := root, ""
		for d := 0; d < 4; d++ {
			name := fmt.Sprintf("d%d_%d", d, i%8)
			path += "/" + name
			dir = mkdir(dir, path, name)
		}
		name := fmt.Sprintf("f%d", i)
		full := path + "/" + name
		h, err := store.GenerateHandle(ctx, share, full)
		require.NoError(tb, err)
		_, id, err := metadata.DecodeFileHandle(h)
		require.NoError(tb, err)

		payload := fmt.Sprintf("payload-%08d", i)
		f := &metadata.File{
			ShareName: share,
			Path:      full,
			FileAttr:  metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o600, PayloadID: metadata.PayloadID(payload), Size: 8},
		}
		f.ID = id
		require.NoError(tb, store.UpdateAttrs(ctx, f))
		require.NoError(tb, store.SetParent(ctx, h, dir))
		require.NoError(tb, store.SetChild(ctx, dir, name, h))
		require.NoError(tb, local.WriteAt(ctx, payload, 0, make([]byte, 8)))
		ids[i] = payload
	}
	return store, local, ids
}

// TestFindStaleSizesReportsOnlyLaggingFiles pins the scan's answer: a file whose
// metadata size already covers the journal is left alone, one that trails it is
// reported with the journal's mark, and a payload with no metadata row (an
// orphan journal entry) is skipped rather than failing the scan.
func TestFindStaleSizesReportsOnlyLaggingFiles(t *testing.T) {
	ctx := context.Background()
	store, local, ids := buildReconcileFixture(t, 4)

	// Grow one file's journal extent past its recorded metadata size, and add a
	// payload the metadata store has never heard of.
	require.NoError(t, local.WriteAt(ctx, ids[2], 8, make([]byte, 24)))
	require.NoError(t, local.WriteAt(ctx, "payload-orphan", 0, make([]byte, 16)))

	stale, err := findStaleSizes(ctx, store, local, local.ListFiles(ctx))
	require.NoError(t, err)
	require.Len(t, stale, 1)
	require.Equal(t, ids[2], stale[0].id)
	require.Equal(t, uint64(32), stale[0].journalSize)
}

// enrichedOnly hides the store's size-only lookup so the scan falls back to the
// enriched GetFileByPayloadID load, which is what share start used to do.
type enrichedOnly struct{ metadata.Store }

// scanSerialEnriched reproduces the share-start scan as it ran before this
// change: one enriched metadata load per locally-resident file, in file order.
func scanSerialEnriched(ctx context.Context, store metadata.Store, localStore local.LocalStore, files []string) (int, error) {
	n := 0
	for _, id := range files {
		journalSize, ok := localStore.FileSize(ctx, id)
		if !ok {
			continue
		}
		f, err := store.GetFileByPayloadID(ctx, metadata.PayloadID(id))
		if err != nil {
			if metadata.IsNotFoundError(err) {
				continue
			}
			return 0, err
		}
		if f == nil || journalSize < 0 || f.Size >= uint64(journalSize) {
			continue
		}
		n++
	}
	return n, nil
}

// BenchmarkShareStartSizeScan measures the share-start size scan on a store
// where every file is locally resident — the pinned, offline-capable
// configuration, and the one where the scan costs the most. "enriched" is the
// per-file load share start used to do; "size-only" reads just the size; both
// then run across workers.
func BenchmarkShareStartSizeScan(b *testing.B) {
	const files = 5000
	ctx := context.Background()
	store, local, _ := buildReconcileFixture(b, files)
	list := local.ListFiles(ctx)

	b.Run("enriched-serial", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := scanSerialEnriched(ctx, store, local, list); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*files), "ns/file")
	})

	for _, tc := range []struct {
		name  string
		store metadata.Store
	}{
		{"enriched", enrichedOnly{store}},
		{"size-only", store},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stale, err := findStaleSizes(ctx, tc.store, local, list)
				if err != nil {
					b.Fatal(err)
				}
				if len(stale) != 0 {
					b.Fatalf("unexpected stale sizes: %d", len(stale))
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*files), "ns/file")
		})
	}
}

// TestFindStaleSizesCoversEveryChunk pins that each worker scans its own slice
// of the file list. The scan hands every worker a batch cut from the same
// backing array, so a batch shared between them would leave whole stretches of
// the list unscanned — and an unscanned file whose size needed growing is a
// read that truncates acknowledged bytes. Growing one file in every chunk,
// including the first, fails loudly if that ever happens.
func TestFindStaleSizesCoversEveryChunk(t *testing.T) {
	const files = 64
	ctx := context.Background()
	store, local, ids := buildReconcileFixture(t, files)

	// ids is in creation order, so growing every fourth file spreads the
	// expected hits across the list however it ends up chunked.
	want := map[string]uint64{}
	for i := 0; i < files; i += 4 {
		require.NoError(t, local.WriteAt(ctx, ids[i], 8, make([]byte, 8)))
		want[ids[i]] = 16
	}

	stale, err := findStaleSizes(ctx, store, local, ids)
	require.NoError(t, err)

	got := map[string]uint64{}
	for _, s := range stale {
		got[s.id] = s.journalSize
	}
	require.Equal(t, want, got)
}
