package memory

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Block Reference Counting Tests
// ============================================================================

func TestObjectStore_BlockRefCount_Basic(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Create a block
	hash := sha256.Sum256([]byte("test data"))
	block := &metadata.ObjectBlock{
		Hash:     hash,
		Size:     1024,
		RefCount: 1,
	}

	// Put the block
	err := store.PutBlock(ctx, block)
	require.NoError(t, err)

	// Verify initial RefCount
	retrieved, err := store.GetBlock(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), retrieved.RefCount)

	// Increment RefCount
	newCount, err := store.IncrementBlockRefCount(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), newCount)

	// Verify increment persisted
	retrieved, err = store.GetBlock(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), retrieved.RefCount)

	// Decrement RefCount
	newCount, err = store.DecrementBlockRefCount(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), newCount)

	// Decrement again
	newCount, err = store.DecrementBlockRefCount(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), newCount)

	// Decrement at zero should stay at zero (no underflow)
	newCount, err = store.DecrementBlockRefCount(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), newCount)
}

func TestObjectStore_BlockRefCount_MultipleIncrements(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	hash := sha256.Sum256([]byte("shared data"))
	block := &metadata.ObjectBlock{
		Hash:     hash,
		Size:     4096,
		RefCount: 1,
	}

	err := store.PutBlock(ctx, block)
	require.NoError(t, err)

	// Simulate 5 files sharing this block
	for i := 0; i < 5; i++ {
		_, err := store.IncrementBlockRefCount(ctx, hash)
		require.NoError(t, err)
	}

	retrieved, err := store.GetBlock(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, uint32(6), retrieved.RefCount) // 1 initial + 5 increments

	// Delete 3 files
	for i := 0; i < 3; i++ {
		_, err := store.DecrementBlockRefCount(ctx, hash)
		require.NoError(t, err)
	}

	retrieved, err = store.GetBlock(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, uint32(3), retrieved.RefCount)
}

func TestObjectStore_BlockRefCount_NotFound(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Try to increment non-existent block
	nonExistentHash := sha256.Sum256([]byte("does not exist"))
	_, err := store.IncrementBlockRefCount(ctx, nonExistentHash)
	assert.Error(t, err)
	assert.ErrorIs(t, err, metadata.ErrBlockNotFound)

	// Try to decrement non-existent block
	_, err = store.DecrementBlockRefCount(ctx, nonExistentHash)
	assert.Error(t, err)
	assert.ErrorIs(t, err, metadata.ErrBlockNotFound)
}

// ============================================================================
// FindBlockByHash Tests (Deduplication)
// ============================================================================

func TestObjectStore_FindBlockByHash_Found(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	hash := sha256.Sum256([]byte("unique content"))
	block := &metadata.ObjectBlock{
		Hash:     hash,
		Size:     2048,
		RefCount: 1,
	}
	block.MarkUploaded()

	err := store.PutBlock(ctx, block)
	require.NoError(t, err)

	// Find the block
	found, err := store.FindBlockByHash(ctx, hash)
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, metadata.ContentHash(hash), found.Hash)
	assert.Equal(t, uint32(2048), found.Size)
	assert.True(t, found.IsUploaded())
}

func TestObjectStore_FindBlockByHash_NotFound(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Find non-existent block - should return nil without error
	nonExistentHash := sha256.Sum256([]byte("not stored"))
	found, err := store.FindBlockByHash(ctx, nonExistentHash)
	require.NoError(t, err)
	assert.Nil(t, found)
}

func TestObjectStore_FindBlockByHash_DedupFlow(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Simulate deduplication flow
	content := []byte("duplicated content across files")
	hash := sha256.Sum256(content)

	// First file writes this block
	block := &metadata.ObjectBlock{
		Hash:     hash,
		Size:     uint32(len(content)),
		RefCount: 1,
	}
	block.MarkUploaded()

	err := store.PutBlock(ctx, block)
	require.NoError(t, err)

	// Second file writes same content - check for existing block
	existing, err := store.FindBlockByHash(ctx, hash)
	require.NoError(t, err)
	require.NotNil(t, existing, "Block should be found for dedup")
	assert.True(t, existing.IsUploaded(), "Block should be uploaded for dedup to skip upload")

	// Dedup: increment RefCount instead of uploading
	newCount, err := store.IncrementBlockRefCount(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), newCount)

	// Third file does the same
	existing, err = store.FindBlockByHash(ctx, hash)
	require.NoError(t, err)
	require.NotNil(t, existing)

	newCount, err = store.IncrementBlockRefCount(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, uint32(3), newCount)
}

// ============================================================================
// Chunk Reference Counting Tests
// ============================================================================

func TestObjectStore_ChunkRefCount_Basic(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	objectID := sha256.Sum256([]byte("object"))
	chunkHash := sha256.Sum256([]byte("chunk"))

	chunk := &metadata.ObjectChunk{
		Hash:     chunkHash,
		ObjectID: objectID,
		Index:    0,
		RefCount: 1,
	}

	err := store.PutChunk(ctx, chunk)
	require.NoError(t, err)

	// Increment
	count, err := store.IncrementChunkRefCount(ctx, chunkHash)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), count)

	// Decrement
	count, err = store.DecrementChunkRefCount(ctx, chunkHash)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), count)
}

// ============================================================================
// Object Reference Counting Tests
// ============================================================================

func TestObjectStore_ObjectRefCount_Basic(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	objectID := sha256.Sum256([]byte("file content"))

	obj := &metadata.Object{
		ID:       objectID,
		Size:     10240,
		RefCount: 1,
	}

	err := store.PutObject(ctx, obj)
	require.NoError(t, err)

	// Increment (simulate hard link)
	count, err := store.IncrementObjectRefCount(ctx, objectID)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), count)

	// Decrement (delete hard link)
	count, err = store.DecrementObjectRefCount(ctx, objectID)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), count)

	// Decrement (delete original file)
	count, err = store.DecrementObjectRefCount(ctx, objectID)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), count)
}

// ============================================================================
// Cascading Delete Tests
// ============================================================================

func TestObjectStore_CascadingDelete(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Create a complete hierarchy: Object -> Chunk -> Block
	objectID := sha256.Sum256([]byte("file"))
	chunkHash := sha256.Sum256([]byte("chunk0"))
	blockHash := sha256.Sum256([]byte("block0"))

	// Create block
	block := &metadata.ObjectBlock{
		Hash:      blockHash,
		ChunkHash: chunkHash,
		Index:     0,
		Size:      4096,
		RefCount:  1,
	}
	err := store.PutBlock(ctx, block)
	require.NoError(t, err)

	// Create chunk
	chunk := &metadata.ObjectChunk{
		Hash:       chunkHash,
		ObjectID:   objectID,
		Index:      0,
		BlockCount: 1,
		RefCount:   1,
	}
	err = store.PutChunk(ctx, chunk)
	require.NoError(t, err)

	// Create object
	obj := &metadata.Object{
		ID:         objectID,
		Size:       4096,
		ChunkCount: 1,
		RefCount:   1,
	}
	err = store.PutObject(ctx, obj)
	require.NoError(t, err)

	// Simulate file deletion - decrement RefCount cascade
	// First, decrement object
	objCount, err := store.DecrementObjectRefCount(ctx, objectID)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), objCount)

	// RefCount is 0, so delete the object and cascade to chunks
	if objCount == 0 {
		// Get chunks for this object
		chunks, err := store.GetChunksByObject(ctx, objectID)
		require.NoError(t, err)
		assert.Len(t, chunks, 1)

		for _, c := range chunks {
			chunkCount, err := store.DecrementChunkRefCount(ctx, c.Hash)
			require.NoError(t, err)

			if chunkCount == 0 {
				// Get blocks for this chunk
				blocks, err := store.GetBlocksByChunk(ctx, c.Hash)
				require.NoError(t, err)

				for _, b := range blocks {
					blockCount, err := store.DecrementBlockRefCount(ctx, b.Hash)
					require.NoError(t, err)

					if blockCount == 0 {
						// Delete block
						err = store.DeleteBlock(ctx, b.Hash)
						require.NoError(t, err)
					}
				}

				// Delete chunk
				err = store.DeleteChunk(ctx, c.Hash)
				require.NoError(t, err)
			}
		}

		// Delete object
		err = store.DeleteObject(ctx, objectID)
		require.NoError(t, err)
	}

	// Verify everything is deleted
	_, err = store.GetObject(ctx, objectID)
	assert.ErrorIs(t, err, metadata.ErrObjectNotFound)

	_, err = store.GetChunk(ctx, chunkHash)
	assert.ErrorIs(t, err, metadata.ErrChunkNotFound)

	_, err = store.GetBlock(ctx, blockHash)
	assert.ErrorIs(t, err, metadata.ErrBlockNotFound)
}

func TestObjectStore_CascadingDelete_SharedBlocks(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Create two objects that share a block
	sharedBlockHash := sha256.Sum256([]byte("shared block"))

	// Create shared block with RefCount 2
	sharedBlock := &metadata.ObjectBlock{
		Hash:     sharedBlockHash,
		Size:     4096,
		RefCount: 2, // Referenced by two chunks
	}
	err := store.PutBlock(ctx, sharedBlock)
	require.NoError(t, err)

	// Delete first file - decrement block RefCount
	count, err := store.DecrementBlockRefCount(ctx, sharedBlockHash)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), count)

	// Block should still exist
	block, err := store.GetBlock(ctx, sharedBlockHash)
	require.NoError(t, err)
	assert.NotNil(t, block)

	// Delete second file - decrement block RefCount
	count, err = store.DecrementBlockRefCount(ctx, sharedBlockHash)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), count)

	// Now we can delete the block
	err = store.DeleteBlock(ctx, sharedBlockHash)
	require.NoError(t, err)

	// Verify block is deleted
	_, err = store.GetBlock(ctx, sharedBlockHash)
	assert.ErrorIs(t, err, metadata.ErrBlockNotFound)
}

// ============================================================================
// MarkBlockUploaded Tests
// ============================================================================

func TestObjectStore_MarkBlockUploaded(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	hash := sha256.Sum256([]byte("data to upload"))
	block := &metadata.ObjectBlock{
		Hash:     hash,
		Size:     1024,
		RefCount: 1,
	}

	err := store.PutBlock(ctx, block)
	require.NoError(t, err)

	// Initially not uploaded
	retrieved, err := store.GetBlock(ctx, hash)
	require.NoError(t, err)
	assert.False(t, retrieved.IsUploaded())

	// Mark as uploaded
	err = store.MarkBlockUploaded(ctx, hash)
	require.NoError(t, err)

	// Now should be uploaded
	retrieved, err = store.GetBlock(ctx, hash)
	require.NoError(t, err)
	assert.True(t, retrieved.IsUploaded())
}

// ============================================================================
// Concurrent Access Tests
// ============================================================================

func TestObjectStore_ConcurrentRefCountUpdates(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	hash := sha256.Sum256([]byte("concurrent"))
	block := &metadata.ObjectBlock{
		Hash:     hash,
		Size:     1024,
		RefCount: 0,
	}

	err := store.PutBlock(ctx, block)
	require.NoError(t, err)

	// Run concurrent increments
	numGoroutines := 100
	done := make(chan bool, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			_, err := store.IncrementBlockRefCount(ctx, hash)
			assert.NoError(t, err)
			done <- true
		}()
	}

	// Wait for all to complete
	for i := 0; i < numGoroutines; i++ {
		<-done
	}

	// Verify final count
	retrieved, err := store.GetBlock(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, uint32(numGoroutines), retrieved.RefCount)
}
