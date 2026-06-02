package memory

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// putFileForBlocksTest is a small in-test helper that stamps a file with
// FileAttr.Blocks via the public Put/Get path on MemoryMetadataStore.
// Returns (handle, file).
//
// Mirror of the storetest createTestFile pattern, kept local because
// storetest helpers are unexported.
func putFileForBlocksTest(
	t *testing.T,
	store *MemoryMetadataStore,
	shareName string,
	name string,
	blocks []block.BlockRef,
) (metadata.FileHandle, *metadata.File) {
	t.Helper()

	ctx := context.Background()

	require.NoError(t, store.CreateShare(ctx, &metadata.Share{Name: shareName}))
	rootAttr := &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755}
	rootFile, err := store.CreateRootDirectory(ctx, shareName, rootAttr)
	require.NoError(t, err)
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	require.NoError(t, err)

	handle, err := store.GenerateHandle(ctx, shareName, "/"+name)
	require.NoError(t, err)
	_, id, err := metadata.DecodeFileHandle(handle)
	require.NoError(t, err)

	now := time.Now().UTC()
	file := &metadata.File{
		ID:        id,
		ShareName: shareName,
		FileAttr: metadata.FileAttr{
			Type:         metadata.FileTypeRegular,
			Mode:         0o644,
			UID:          1000,
			GID:          1000,
			Size:         uint64(len(blocks)) * 4 * 1024 * 1024,
			Mtime:        now,
			Ctime:        now,
			Atime:        now,
			CreationTime: now,
			Blocks:       blocks,
		},
	}
	require.NoError(t, store.PutFile(ctx, file))
	require.NoError(t, store.SetParent(ctx, handle, rootHandle))
	require.NoError(t, store.SetChild(ctx, rootHandle, name, handle))
	return handle, file
}

// makeBlock returns a BlockRef with a deterministic ContentHash whose first
// byte is `seed` and offset/size pegged to the block index.
func makeBlock(seed byte, idx int) block.BlockRef {
	var h block.ContentHash
	h[0] = seed
	return block.BlockRef{
		Hash:   h,
		Offset: uint64(idx) * 4 * 1024 * 1024,
		Size:   4 * 1024 * 1024,
	}
}

// TestMemoryStore_PutFile_BlocksDeepCopy verifies that PutFile deep-copies
// the caller's []BlockRef so a subsequent caller-side mutation cannot
// observably mutate the stored view (T-12-09 mitigation).
func TestMemoryStore_PutFile_BlocksDeepCopy(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	original := []block.BlockRef{
		makeBlock(0xAA, 0),
		makeBlock(0xBB, 1),
		makeBlock(0xCC, 2),
	}
	handle, _ := putFileForBlocksTest(t, store, "/put-deep", "put.bin", original)

	// Mutate the caller-side slice AFTER PutFile returned.
	mutated := original
	mutated[0].Hash[0] = 0xFF
	mutated[1].Offset = 999
	mutated[2].Size = 1

	// Read back via GetFile — stored Blocks must be unchanged.
	got, err := store.GetFile(ctx, handle)
	require.NoError(t, err)
	require.Len(t, got.Blocks, 3)

	assert.Equal(t, byte(0xAA), got.Blocks[0].Hash[0],
		"PutFile must deep-copy: caller-side hash mutation leaked into stored state")
	assert.Equal(t, uint64(4*1024*1024), got.Blocks[1].Offset,
		"PutFile must deep-copy: caller-side offset mutation leaked into stored state")
	assert.Equal(t, uint32(4*1024*1024), got.Blocks[2].Size,
		"PutFile must deep-copy: caller-side size mutation leaked into stored state")
}

// TestMemoryStore_GetFile_BlocksDeepCopy verifies that GetFile returns a
// deep copy of the stored []BlockRef so a subsequent caller-side mutation
// of the returned slice cannot observably mutate the stored view.
func TestMemoryStore_GetFile_BlocksDeepCopy(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	original := []block.BlockRef{
		makeBlock(0x11, 0),
		makeBlock(0x22, 1),
	}
	handle, _ := putFileForBlocksTest(t, store, "/get-deep", "get.bin", original)

	first, err := store.GetFile(ctx, handle)
	require.NoError(t, err)
	require.Len(t, first.Blocks, 2)

	// Mutate the returned slice's elements.
	first.Blocks[0].Hash[0] = 0xFF
	first.Blocks[1].Offset = 999

	// Get again — must observe the original stored view.
	second, err := store.GetFile(ctx, handle)
	require.NoError(t, err)
	require.Len(t, second.Blocks, 2)

	assert.Equal(t, byte(0x11), second.Blocks[0].Hash[0],
		"GetFile must deep-copy: caller-side hash mutation of returned slice leaked into stored state")
	assert.Equal(t, uint64(4*1024*1024), second.Blocks[1].Offset,
		"GetFile must deep-copy: caller-side offset mutation of returned slice leaked into stored state")
}

// TestMemoryStore_PutGetFile_NilBlocks verifies that a nil Blocks slice
// round-trips as nil/empty without panic.
func TestMemoryStore_PutGetFile_NilBlocks(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	handle, _ := putFileForBlocksTest(t, store, "/nil-blocks", "nil.bin", nil)

	got, err := store.GetFile(ctx, handle)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 0, len(got.Blocks),
		"nil/empty Blocks must round-trip as zero-length without panic")

	// Direct sanity: re-read is still safe across calls.
	again, err := store.GetFile(ctx, handle)
	require.NoError(t, err)
	assert.Equal(t, 0, len(again.Blocks))

	// Avoid unused var on uuid import in case future edits depend on it.
	_ = uuid.Nil
}
