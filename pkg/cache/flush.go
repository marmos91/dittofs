package cache

import (
	"cmp"
	"context"
	"slices"
)

// ============================================================================
// Flush Coordination
// ============================================================================

// GetDirtyBlocks returns all pending (unflushed) blocks for a file, ready for upload.
//
// Returns blocks sorted by (ChunkIndex, BlockIndex).
// The returned PendingBlock.Data references the cache's internal buffer - do not modify.
func (c *Cache) GetDirtyBlocks(ctx context.Context, payloadID string) ([]PendingBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return nil, ErrCacheClosed
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(payloadID)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	if len(entry.chunks) == 0 {
		return nil, ErrFileNotInCache
	}

	var result []PendingBlock
	for chunkIdx, chunk := range entry.chunks {
		for blockIdx, blk := range chunk.blocks {
			// Check context between blocks to allow cancellation during large files
			if err := ctx.Err(); err != nil {
				return nil, err
			}

			if blk.state != BlockStatePending {
				continue
			}

			if blk.data == nil {
				continue
			}

			result = append(result, PendingBlock{
				ChunkIndex: chunkIdx,
				BlockIndex: blockIdx,
				Data:       blk.data,
				Coverage:   blk.coverage,
				DataSize:   blk.dataSize,
			})
		}
	}

	slices.SortFunc(result, func(a, b PendingBlock) int {
		return cmp.Or(cmp.Compare(a.ChunkIndex, b.ChunkIndex), cmp.Compare(a.BlockIndex, b.BlockIndex))
	})

	return result, nil
}

// MarkBlockUploaded marks a block as successfully uploaded to the block store.
//
// This should be called by the TransferManager after successfully uploading a block.
// The block transitions from BlockStatePending to BlockStateUploaded, making it
// eligible for LRU eviction when cache pressure requires freeing memory.
//
// Parameters:
//   - payloadID: Unique identifier for the file content
//   - chunkIdx: The chunk index containing the block
//   - blockIdx: The block index within the chunk
//
// Returns true if the block was found and marked.
func (c *Cache) MarkBlockUploaded(ctx context.Context, payloadID string, chunkIdx, blockIdx uint32) bool {
	if err := ctx.Err(); err != nil {
		return false
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return false
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(payloadID)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	chunk, exists := entry.chunks[chunkIdx]
	if !exists {
		return false
	}

	blk, exists := chunk.blocks[blockIdx]
	if !exists {
		return false
	}

	if blk.state == BlockStatePending || blk.state == BlockStateUploading {
		blk.state = BlockStateUploaded
		// Decrement pending size - block is no longer pending
		c.pendingSize.Add(^uint64(BlockSize - 1)) // Atomic subtract
		return true
	}

	return false
}

// MarkBlockUploading marks a block as currently being uploaded.
//
// This prevents eviction during upload and indicates upload is in progress.
//
// Parameters:
//   - payloadID: Unique identifier for the file content
//   - chunkIdx: The chunk index containing the block
//   - blockIdx: The block index within the chunk
//
// Returns true if the block was found and marked.
func (c *Cache) MarkBlockUploading(ctx context.Context, payloadID string, chunkIdx, blockIdx uint32) bool {
	if err := ctx.Err(); err != nil {
		return false
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return false
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(payloadID)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	chunk, exists := entry.chunks[chunkIdx]
	if !exists {
		return false
	}

	blk, exists := chunk.blocks[blockIdx]
	if !exists {
		return false
	}

	if blk.state == BlockStatePending {
		blk.state = BlockStateUploading
		return true
	}

	return false
}

// GetBlockData returns the data for a specific block.
//
// This is used by the TransferManager to get block data for upload.
// The returned data references the cache's internal buffer - do not modify.
//
// Parameters:
//   - payloadID: Unique identifier for the file content
//   - chunkIdx: The chunk index containing the block
//   - blockIdx: The block index within the chunk
//
// Returns the block data and its actual size, or nil if not found.
func (c *Cache) GetBlockData(ctx context.Context, payloadID string, chunkIdx, blockIdx uint32) ([]byte, uint32, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return nil, 0, ErrCacheClosed
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(payloadID)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	chunk, exists := entry.chunks[chunkIdx]
	if !exists {
		return nil, 0, ErrBlockNotFound
	}

	blk, exists := chunk.blocks[blockIdx]
	if !exists || blk.data == nil {
		return nil, 0, ErrBlockNotFound
	}

	return blk.data, blk.dataSize, nil
}
