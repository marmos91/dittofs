package memory

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// FileBlock Reference Counting Tests
// ============================================================================

func TestFileBlockStore_RefCount_Basic(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	id := uuid.New().String()
	hash := sha256.Sum256([]byte("test data"))
	block := &metadata.FileBlock{
		ID:       id,
		Hash:     hash,
		DataSize: 1024,
		RefCount: 1,
	}

	err := store.PutFileBlock(ctx, block)
	require.NoError(t, err)

	// Verify initial RefCount
	retrieved, err := store.GetFileBlock(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), retrieved.RefCount)

	// Increment RefCount
	err = store.IncrementRefCount(ctx, id)
	require.NoError(t, err)

	// Verify increment persisted
	retrieved, err = store.GetFileBlock(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), retrieved.RefCount)

	// Decrement RefCount
	newCount, err := store.DecrementRefCount(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), newCount)

	// Decrement again
	newCount, err = store.DecrementRefCount(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), newCount)

	// Decrement at zero should stay at zero (no underflow)
	newCount, err = store.DecrementRefCount(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), newCount)
}

func TestFileBlockStore_RefCount_MultipleIncrements(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	id := uuid.New().String()
	hash := sha256.Sum256([]byte("shared data"))
	block := &metadata.FileBlock{
		ID:       id,
		Hash:     hash,
		DataSize: 4096,
		RefCount: 1,
	}

	err := store.PutFileBlock(ctx, block)
	require.NoError(t, err)

	// Simulate 5 files sharing this block
	for i := 0; i < 5; i++ {
		err := store.IncrementRefCount(ctx, id)
		require.NoError(t, err)
	}

	retrieved, err := store.GetFileBlock(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, uint32(6), retrieved.RefCount) // 1 initial + 5 increments

	// Delete 3 files
	for i := 0; i < 3; i++ {
		_, err := store.DecrementRefCount(ctx, id)
		require.NoError(t, err)
	}

	retrieved, err = store.GetFileBlock(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, uint32(3), retrieved.RefCount)
}

func TestFileBlockStore_RefCount_NotFound(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Try to increment non-existent block
	err := store.IncrementRefCount(ctx, "does-not-exist")
	assert.Error(t, err)
	assert.ErrorIs(t, err, metadata.ErrFileBlockNotFound)

	// Try to decrement non-existent block
	_, err = store.DecrementRefCount(ctx, "does-not-exist")
	assert.Error(t, err)
	assert.ErrorIs(t, err, metadata.ErrFileBlockNotFound)
}

// ============================================================================
// FindFileBlockByHash Tests (Deduplication)
// ============================================================================

func TestFileBlockStore_FindByHash_Found(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	id := uuid.New().String()
	hash := sha256.Sum256([]byte("unique content"))
	block := &metadata.FileBlock{
		ID:            id,
		Hash:          hash,
		DataSize:      2048,
		RefCount:      1,
		BlockStoreKey: "s3://bucket/key",
	}

	err := store.PutFileBlock(ctx, block)
	require.NoError(t, err)

	// Find the block by hash
	found, err := store.FindFileBlockByHash(ctx, hash)
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, id, found.ID)
	assert.Equal(t, metadata.ContentHash(hash), found.Hash)
	assert.Equal(t, uint32(2048), found.DataSize)
	assert.True(t, found.IsRemote())
}

func TestFileBlockStore_FindByHash_NotFound(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Find non-existent block - should return nil without error
	nonExistentHash := sha256.Sum256([]byte("not stored"))
	found, err := store.FindFileBlockByHash(ctx, nonExistentHash)
	require.NoError(t, err)
	assert.Nil(t, found)
}

func TestFileBlockStore_DedupFlow(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Simulate deduplication flow
	content := []byte("duplicated content across files")
	hash := sha256.Sum256(content)

	// First file writes this block
	id1 := uuid.New().String()
	block := &metadata.FileBlock{
		ID:            id1,
		Hash:          hash,
		DataSize:      uint32(len(content)),
		RefCount:      1,
		BlockStoreKey: "s3://bucket/key",
	}

	err := store.PutFileBlock(ctx, block)
	require.NoError(t, err)

	// Second file writes same content - check for existing block
	existing, err := store.FindFileBlockByHash(ctx, hash)
	require.NoError(t, err)
	require.NotNil(t, existing, "Block should be found for dedup")
	assert.True(t, existing.IsRemote(), "Block should be remote for dedup to skip upload")

	// Dedup: increment RefCount instead of uploading
	err = store.IncrementRefCount(ctx, existing.ID)
	require.NoError(t, err)

	retrieved, err := store.GetFileBlock(ctx, id1)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), retrieved.RefCount)
}

// ============================================================================
// Delete Tests
// ============================================================================

func TestFileBlockStore_Delete(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	id := uuid.New().String()
	hash := sha256.Sum256([]byte("data"))
	block := &metadata.FileBlock{
		ID:       id,
		Hash:     hash,
		DataSize: 1024,
		RefCount: 1,
	}

	err := store.PutFileBlock(ctx, block)
	require.NoError(t, err)

	// Delete block
	err = store.DeleteFileBlock(ctx, id)
	require.NoError(t, err)

	// Verify deleted
	_, err = store.GetFileBlock(ctx, id)
	assert.ErrorIs(t, err, metadata.ErrFileBlockNotFound)

	// Hash index should also be cleared
	found, err := store.FindFileBlockByHash(ctx, hash)
	require.NoError(t, err)
	assert.Nil(t, found)
}

// ============================================================================
// Concurrent Access Tests
// ============================================================================

func TestFileBlockStore_ConcurrentRefCountUpdates(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	id := uuid.New().String()
	hash := sha256.Sum256([]byte("concurrent"))
	block := &metadata.FileBlock{
		ID:       id,
		Hash:     hash,
		DataSize: 1024,
		RefCount: 0,
	}

	err := store.PutFileBlock(ctx, block)
	require.NoError(t, err)

	// Run concurrent increments
	numGoroutines := 100
	done := make(chan bool, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			err := store.IncrementRefCount(ctx, id)
			assert.NoError(t, err)
			done <- true
		}()
	}

	// Wait for all to complete
	for i := 0; i < numGoroutines; i++ {
		<-done
	}

	// Verify final count
	retrieved, err := store.GetFileBlock(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, uint32(numGoroutines), retrieved.RefCount)
}
