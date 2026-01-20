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
// If WAL persistence is enabled, the uploaded state is recorded to the WAL so that
// on recovery, the block won't be re-uploaded unnecessarily.
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

		// If buffer was detached (nil), also decrement totalSize since memory is released
		if blk.data == nil {
			c.totalSize.Add(^uint64(BlockSize - 1)) // Atomic subtract
		}

		// Record uploaded state in WAL for crash recovery.
		// On recovery, blocks with this marker won't be re-uploaded.
		//
		// Safety: We release the lock during WAL write to avoid holding it during I/O.
		// This is safe because:
		// 1. The state transition to Uploaded is already complete
		// 2. The WAL append is idempotent (duplicate markers are harmless)
		// 3. We don't access block state after re-acquiring the lock
		if c.persister != nil {
			entry.mu.Unlock()
			_ = c.persister.AppendBlockUploaded(payloadID, chunkIdx, blockIdx)
			entry.mu.Lock()
		}

		return true
	}

	return false
}

// MarkBlockPending reverts a block from Uploading state back to Pending.
//
// This is used for error recovery when an upload fails, allowing the block
// to be retried in a future flush operation.
//
// Parameters:
//   - payloadID: Unique identifier for the file content
//   - chunkIdx: The chunk index containing the block
//   - blockIdx: The block index within the chunk
//
// Returns true if the block was found and reverted.
func (c *Cache) MarkBlockPending(ctx context.Context, payloadID string, chunkIdx, blockIdx uint32) bool {
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

	if blk.state == BlockStateUploading {
		blk.state = BlockStatePending
		return true
	}

	return false
}

// MarkBlockUploading marks a block as currently being uploaded.
//
// This prevents eviction during upload and indicates upload is in progress.
// Used for atomic "claim" semantics to prevent concurrent uploads of the same block.
//
// Parameters:
//   - payloadID: Unique identifier for the file content
//   - chunkIdx: The chunk index containing the block
//   - blockIdx: The block index within the chunk
//
// Returns true if the block was found and marked (state was Pending).
// Returns false if the block doesn't exist or is already Uploading/Uploaded.
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

// DetachBlockForUpload atomically claims a block for upload and detaches its buffer.
//
// This is a zero-copy operation that transfers ownership of the block's data buffer
// to the caller. The block is marked as Uploading and its data pointer is set to nil.
// This prevents data corruption from concurrent writes during upload.
//
// The caller takes ownership of the returned buffer and is responsible for:
//   - Uploading the data to the block store
//   - Returning the buffer to a pool after upload
//   - Calling RestoreBlockBuffer on failure, or MarkBlockUploaded on success
//
// Parameters:
//   - payloadID: Unique identifier for the file content
//   - chunkIdx: The chunk index containing the block
//   - blockIdx: The block index within the chunk
//
// Returns:
//   - data: The detached buffer (caller takes ownership), nil if block not found/claimable
//   - dataSize: Actual data size in the buffer
//   - ok: true if block was successfully claimed and detached
func (c *Cache) DetachBlockForUpload(ctx context.Context, payloadID string, chunkIdx, blockIdx uint32) (data []byte, dataSize uint32, ok bool) {
	if err := ctx.Err(); err != nil {
		return nil, 0, false
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return nil, 0, false
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(payloadID)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	chunk, exists := entry.chunks[chunkIdx]
	if !exists {
		return nil, 0, false
	}

	blk, exists := chunk.blocks[blockIdx]
	if !exists {
		return nil, 0, false
	}

	// Can only detach Pending blocks
	if blk.state != BlockStatePending {
		return nil, 0, false
	}

	if blk.data == nil {
		return nil, 0, false
	}

	// Move the buffer out (zero-copy transfer of ownership)
	data = blk.data
	dataSize = blk.dataSize
	blk.data = nil // Detach - caller now owns the buffer
	blk.state = BlockStateUploading

	return data, dataSize, true
}

// RestoreBlockBuffer restores a detached buffer back to a block after upload failure.
//
// This is used for error recovery when an upload fails. The buffer is restored
// and the block is reverted to Pending state so it can be retried.
//
// Parameters:
//   - payloadID: Unique identifier for the file content
//   - chunkIdx: The chunk index containing the block
//   - blockIdx: The block index within the chunk
//   - data: The buffer to restore (ownership transfers back to cache)
//
// Returns true if the buffer was restored successfully.
func (c *Cache) RestoreBlockBuffer(ctx context.Context, payloadID string, chunkIdx, blockIdx uint32, data []byte) bool {
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

	// Only restore to Uploading blocks (the expected state after detach)
	if blk.state != BlockStateUploading {
		return false
	}

	// Restore the buffer
	blk.data = data
	blk.state = BlockStatePending

	return true
}
