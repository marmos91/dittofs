package badger

import (
	"context"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// readManifest reads the fm:<uuid> manifest key directly: its badger commit
// version (which advances on every write, even a same-value rewrite), the
// decoded block list, and whether the key exists. The version is what proves an
// attr-only write did NOT touch the manifest.
func readManifest(t *testing.T, s *BadgerMetadataStore, id [16]byte) (version uint64, blocks []block.ChunkRef, exists bool) {
	t.Helper()
	require.NoError(t, s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(keyFileManifest(id))
		if err == badgerdb.ErrKeyNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		exists = true
		version = item.Version()
		return item.Value(func(val []byte) error {
			blocks, err = decodeManifest(val)
			return err
		})
	}))
	return version, blocks, exists
}

// mkChunkedFile creates a regular file carrying nChunks 1 MiB blocks and returns
// its handle. The manifest is marked dirty so PutFile persists it to fm:.
func mkChunkedFile(t *testing.T, store *BadgerMetadataStore, share string, dir metadata.FileHandle, name, path string, nChunks int) metadata.FileHandle {
	t.Helper()
	ctx := context.Background()
	handle, err := store.GenerateHandle(ctx, share, path)
	require.NoError(t, err)
	_, id, err := metadata.DecodeFileHandle(handle)
	require.NoError(t, err)

	blocks := make([]block.ChunkRef, nChunks)
	for i := range blocks {
		var h block.ContentHash
		h[0], h[1] = byte(i), byte(i>>8)
		blocks[i] = block.ChunkRef{Hash: h, Offset: uint64(i) << 20, Size: 1 << 20}
	}

	file := &metadata.File{
		ShareName: share,
		Path:      path,
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o600, UID: 1000, GID: 1000,
			Size:        uint64(nChunks) << 20,
			PayloadID:   metadata.PayloadID(path),
			Blocks:      blocks,
			BlocksDirty: true,
		},
	}
	file.ID = id
	require.NoError(t, store.PutFile(ctx, file))
	require.NoError(t, store.SetParent(ctx, handle, dir))
	require.NoError(t, store.SetChild(ctx, dir, name, handle))
	return handle
}

// TestManifestSplit_AttrOnlyWriteLeavesManifestUntouched is the load-bearing
// assertion of the fm: split: a chmod / utimes on a multi-chunk file must not
// rewrite the manifest. The fm: key's commit version is byte-identical before
// and after, and the block list is unchanged.
func TestManifestSplit_AttrOnlyWriteLeavesManifestUntouched(t *testing.T) {
	ctx := context.Background()
	store, err := NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	root := mkPayloadShare(t, store, "/s")
	handle := mkChunkedFile(t, store, "/s", root, "big.bin", "/big.bin", 64)
	_, id, err := metadata.DecodeFileHandle(handle)
	require.NoError(t, err)

	v0, blocks0, ok := readManifest(t, store, id)
	require.True(t, ok, "manifest must be persisted for a carved file")
	require.Len(t, blocks0, 64)

	// Round-trip: GetFile returns the same manifest that was stored.
	got, err := store.GetFile(ctx, handle)
	require.NoError(t, err)
	require.Equal(t, blocks0, got.Blocks, "manifest must round-trip through the split")

	// chmod: attr-only mutation, BlocksDirty stays false.
	got.Mode = 0o644
	require.NoError(t, store.PutFile(ctx, got))

	v1, blocks1, ok := readManifest(t, store, id)
	require.True(t, ok)
	require.Equal(t, v0, v1, "chmod must not rewrite the manifest (fm: version unchanged)")
	require.Equal(t, blocks0, blocks1)

	// utimes: another attr-only mutation.
	got2, err := store.GetFile(ctx, handle)
	require.NoError(t, err)
	got2.Mtime = got2.Mtime.Add(1)
	require.NoError(t, store.PutFile(ctx, got2))

	v2, _, ok := readManifest(t, store, id)
	require.True(t, ok)
	require.Equal(t, v0, v2, "utimes must not rewrite the manifest")
}

// TestManifestSplit_GetByPayloadIDReturnsManifest guards the flush coordinator's
// contract: badger's GetFileByPayloadID returns the manifest inline, including on
// the pl: secondary-index fast path.
func TestManifestSplit_GetByPayloadIDReturnsManifest(t *testing.T) {
	ctx := context.Background()
	store, err := NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	root := mkPayloadShare(t, store, "/s")
	mkChunkedFile(t, store, "/s", root, "big.bin", "/big.bin", 12)

	got, err := store.GetFileByPayloadID(ctx, metadata.PayloadID("/big.bin"))
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Blocks, 12, "GetFileByPayloadID must return the manifest inline")
}

// TestManifestSplit_CarveWritesManifest confirms the gate still persists a
// genuinely changed manifest (a re-carve marks BlocksDirty).
func TestManifestSplit_CarveWritesManifest(t *testing.T) {
	ctx := context.Background()
	store, err := NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	root := mkPayloadShare(t, store, "/s")
	handle := mkChunkedFile(t, store, "/s", root, "big.bin", "/big.bin", 8)
	_, id, err := metadata.DecodeFileHandle(handle)
	require.NoError(t, err)

	v0, _, ok := readManifest(t, store, id)
	require.True(t, ok)

	got, err := store.GetFile(ctx, handle)
	require.NoError(t, err)
	var h block.ContentHash
	h[0] = 0xEE
	got.Blocks = append(got.Blocks, block.ChunkRef{Hash: h, Offset: 8 << 20, Size: 1 << 20})
	got.Size = 9 << 20
	got.BlocksDirty = true
	require.NoError(t, store.PutFile(ctx, got))

	v1, blocks1, ok := readManifest(t, store, id)
	require.True(t, ok)
	require.NotEqual(t, v0, v1, "a carve must rewrite the manifest")
	require.Len(t, blocks1, 9)

	reloaded, err := store.GetFile(ctx, handle)
	require.NoError(t, err)
	require.Equal(t, blocks1, reloaded.Blocks)
}

// TestManifestSplit_TruncatePrunes confirms a truncate prunes the manifest, and
// truncating to zero removes the fm: key entirely.
func TestManifestSplit_TruncatePrunes(t *testing.T) {
	ctx := context.Background()
	store, err := NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	root := mkPayloadShare(t, store, "/s")
	handle := mkChunkedFile(t, store, "/s", root, "big.bin", "/big.bin", 16)
	_, id, err := metadata.DecodeFileHandle(handle)
	require.NoError(t, err)

	// Truncate to 4 MiB: prune to the first 4 chunks.
	got, err := store.GetFile(ctx, handle)
	require.NoError(t, err)
	got.Blocks = block.PruneChunkRefsToSize(got.Blocks, 4<<20)
	got.Size = 4 << 20
	got.BlocksDirty = true
	require.NoError(t, store.PutFile(ctx, got))

	_, blocks, ok := readManifest(t, store, id)
	require.True(t, ok)
	require.Len(t, blocks, 4, "truncate must prune the manifest")

	// Truncate to zero: the manifest key must be removed.
	got, err = store.GetFile(ctx, handle)
	require.NoError(t, err)
	got.Blocks = block.PruneChunkRefsToSize(got.Blocks, 0)
	got.Size = 0
	got.BlocksDirty = true
	require.NoError(t, store.PutFile(ctx, got))

	_, _, ok = readManifest(t, store, id)
	require.False(t, ok, "truncate-to-zero must delete the fm: key")

	reloaded, err := store.GetFile(ctx, handle)
	require.NoError(t, err)
	require.Empty(t, reloaded.Blocks)
}
