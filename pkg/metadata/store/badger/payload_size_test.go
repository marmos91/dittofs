package badger

import (
	"context"
	"fmt"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// mkNestedFile creates a regular file at the given depth below the share root,
// materializing the intervening directories, so the derived path
// GetFileByPayloadID builds has real parent edges to walk.
func mkNestedFile(t testing.TB, store *BadgerMetadataStore, shareName string, root metadata.FileHandle, depth, idx int, pid metadata.PayloadID, size uint64) {
	t.Helper()
	ctx := context.Background()

	dir, path := root, ""
	for d := 0; d < depth; d++ {
		name := fmt.Sprintf("d%d_%d", d, idx%8)
		path += "/" + name
		h, err := store.GenerateHandle(ctx, shareName, path)
		require.NoError(t, err)
		_, id, err := metadata.DecodeFileHandle(h)
		require.NoError(t, err)
		f := &metadata.File{ShareName: shareName, Path: path, FileAttr: metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755}}
		f.ID = id
		require.NoError(t, store.UpdateAttrs(ctx, f))
		require.NoError(t, store.SetParent(ctx, h, dir))
		require.NoError(t, store.SetChild(ctx, dir, name, h))
		dir = h
	}

	name := fmt.Sprintf("f%d", idx)
	full := path + "/" + name
	h, err := store.GenerateHandle(ctx, shareName, full)
	require.NoError(t, err)
	_, id, err := metadata.DecodeFileHandle(h)
	require.NoError(t, err)
	f := &metadata.File{
		ShareName: shareName,
		Path:      full,
		FileAttr:  metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o600, UID: 1000, GID: 1000, PayloadID: pid, Size: size},
	}
	f.ID = id
	require.NoError(t, store.UpdateAttrs(ctx, f))
	require.NoError(t, store.SetParent(ctx, h, dir))
	require.NoError(t, store.SetChild(ctx, dir, name, h))
}

func newSizeTestStore(t testing.TB) *BadgerMetadataStore {
	t.Helper()
	store, err := NewBadgerMetadataStoreWithDefaults(context.Background(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestFileSizeByPayloadIDMatchesGetFileByPayloadID pins the fast size lookup to
// the answer the enriched load gives, across the cases the reconcile hits: a
// live row, an unknown payload (orphan journal entry), the empty payload, and a
// stale pl: entry left behind by a row that no longer claims the payload.
func TestFileSizeByPayloadIDMatchesGetFileByPayloadID(t *testing.T) {
	ctx := context.Background()
	store := newSizeTestStore(t)
	root := mkPayloadShare(t, store, "sizes")

	mkNestedFile(t, store, "sizes", root, 4, 0, "payload-live", 4096)

	t.Run("live row", func(t *testing.T) {
		size, found, err := store.FileSizeByPayloadID(ctx, "payload-live")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, uint64(4096), size)

		f, err := store.GetFileByPayloadID(ctx, "payload-live")
		require.NoError(t, err)
		require.Equal(t, f.Size, size)
	})

	t.Run("unknown payload", func(t *testing.T) {
		_, found, err := store.FileSizeByPayloadID(ctx, "payload-absent")
		require.NoError(t, err)
		require.False(t, found)
	})

	t.Run("empty payload", func(t *testing.T) {
		_, found, err := store.FileSizeByPayloadID(ctx, "")
		require.NoError(t, err)
		require.False(t, found)
	})

	t.Run("stale index entry falls back", func(t *testing.T) {
		// Point pl:payload-stale at a file ID that has no row: the fast path
		// must not report not-found on its own, it must ask the enriched load.
		require.NoError(t, store.db.Update(func(txn *badgerdb.Txn) error {
			raw, err := uuid.New().MarshalBinary()
			if err != nil {
				return err
			}
			return txn.Set(keyPayloadID("payload-stale"), raw)
		}))
		_, found, err := store.FileSizeByPayloadID(ctx, "payload-stale")
		require.NoError(t, err)
		require.False(t, found)
	})
}

// BenchmarkSizeByPayloadID contrasts the two reads share start can use to
// reconcile one file's size. The gap is the enrichment the reconcile discards:
// the link count, the chunk manifest, and the derived path, which walks the
// parent chain with two point reads per component.
func BenchmarkSizeByPayloadID(b *testing.B) {
	const files = 2000
	ctx := context.Background()
	store := newSizeTestStore(b)
	root := mkPayloadShare(b, store, "bench")

	ids := make([]metadata.PayloadID, files)
	for i := range ids {
		ids[i] = metadata.PayloadID(fmt.Sprintf("payload-%06d", i))
		mkNestedFile(b, store, "bench", root, 4, i, ids[i], uint64(i))
	}

	b.Run("GetFileByPayloadID", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := store.GetFileByPayloadID(ctx, ids[i%files]); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("FileSizeByPayloadID", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, err := store.FileSizeByPayloadID(ctx, ids[i%files]); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// TestFileSizeByPayloadIDResolvesLegacyRows pins the assumption that lets the
// fast path treat a pl: index miss as "no row holds this payload" instead of
// scanning: the open-time backfill indexes rows written before that index
// existed, so by the time any caller runs there is nothing left to scan for. If
// that ever stopped holding, share start would skip a file whose size needed
// growing and reads would truncate at the stale size.
func TestFileSizeByPayloadIDResolvesLegacyRows(t *testing.T) {
	dir := t.TempDir()
	want := seedLegacyFiles(t, dir, "legacy-a", "legacy-b")

	store := openStore(t, dir)
	defer func() { _ = store.Close() }()

	for _, f := range want {
		size, found, err := store.FileSizeByPayloadID(context.Background(), f.PayloadID)
		require.NoError(t, err)
		require.True(t, found, "payload %q not resolved after open", f.PayloadID)
		require.Equal(t, f.Size, size)
	}
}
