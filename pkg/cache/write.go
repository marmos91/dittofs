package cache

import (
	"context"

	"github.com/marmos91/dittofs/pkg/cache/wal"
	"github.com/marmos91/dittofs/pkg/payload/block"
)

// ============================================================================
// Write Operations
// ============================================================================

// writeOptions controls the behavior of writeAtInternal.
type writeOptions struct {
	// isDownloaded indicates data is from block store (already persisted).
	// When true:
	//   - Skip pending size backpressure check
	//   - New blocks start as Uploaded (evictable)
	//   - Skip WAL write (data already safe in block store)
	//   - Preserve existing block state (don't re-dirty)
	isDownloaded bool
}

// WriteAt writes data to the cache at the specified chunk and offset.
//
// This is the primary write path for all file data. Data is written directly
// into 4MB block buffers at the correct position. The coverage bitmap tracks
// which bytes have been written for sparse file support.
//
// Block Buffer Model:
// Data is written directly to the target position in the block buffer.
// Overlapping writes simply overwrite previous data (newest-wins semantics).
//
// Memory Tracking:
// The cache tracks actual memory allocation (BlockSize per block buffer), not
// just bytes written. This ensures accurate backpressure for OOM prevention.
//
// Parameters:
//   - payloadID: Unique identifier for the file content
//   - chunkIdx: Which 64MB chunk this write belongs to
//   - data: The bytes to write (copied into cache, safe to modify after call)
//   - offset: Byte offset within the chunk (0 to ChunkSize-1)
//
// Errors:
//   - ErrInvalidOffset: offset + len(data) exceeds ChunkSize
//   - ErrCacheClosed: cache has been closed
//   - ErrCacheFull: cache is full of pending data that cannot be evicted
//   - context.Canceled/DeadlineExceeded: context was cancelled
func (c *Cache) WriteAt(ctx context.Context, payloadID string, chunkIdx uint32, data []byte, offset uint32) error {
	return c.writeAtInternal(ctx, payloadID, chunkIdx, data, offset, writeOptions{isDownloaded: false})
}

// WriteDownloaded writes data that was downloaded from the block store.
//
// Unlike WriteAt(), this method:
//   - Marks blocks as Uploaded (evictable), not Pending
//   - Does NOT count against pendingSize (it's not dirty data)
//   - Does NOT write to WAL (data is already safe in block store)
//   - CAN evict other uploaded blocks to make room (downloaded data is evictable)
//
// This is used by the TransferManager when downloading blocks from S3 on cache miss.
// Downloaded data is already persisted in the block store, so it can be evicted
// immediately if cache pressure requires it.
//
// Parameters:
//   - payloadID: Unique identifier for the file content
//   - chunkIdx: Which 64MB chunk this write belongs to
//   - data: The bytes to write (copied into cache, safe to modify after call)
//   - offset: Byte offset within the chunk (0 to ChunkSize-1)
//
// Errors:
//   - ErrInvalidOffset: offset + len(data) exceeds ChunkSize
//   - ErrCacheClosed: cache has been closed
//   - ErrCacheFull: cache is completely full even after eviction
//   - context.Canceled/DeadlineExceeded: context was cancelled
func (c *Cache) WriteDownloaded(ctx context.Context, payloadID string, chunkIdx uint32, data []byte, offset uint32) error {
	return c.writeAtInternal(ctx, payloadID, chunkIdx, data, offset, writeOptions{isDownloaded: true})
}

// writeAtInternal is the shared implementation for WriteAt and WriteDownloaded.
func (c *Cache) writeAtInternal(ctx context.Context, payloadID string, chunkIdx uint32, data []byte, offset uint32, opts writeOptions) error {
	if err := c.checkClosed(ctx); err != nil {
		return err
	}

	// Fast path: nothing to write
	if len(data) == 0 {
		return nil
	}

	// Validate parameters
	if offset+uint32(len(data)) > ChunkSize {
		return ErrInvalidOffset
	}

	// Calculate which blocks this write spans
	startBlock := block.IndexForOffset(offset)
	endBlock := block.IndexForOffset(offset + uint32(len(data)) - 1)

	entry := c.getFileEntry(payloadID)
	entry.mu.Lock()

	// Calculate how many NEW blocks will be created (for memory tracking).
	newMemory := c.countNewBlockMemory(entry, chunkIdx, startBlock, endBlock)

	// Enforce maxSize by evicting LRU uploaded blocks if needed.
	if c.maxSize > 0 && newMemory > 0 {
		if c.totalSize.Load()+newMemory > c.maxSize {
			// Release lock to evict (eviction needs to lock other entries)
			entry.mu.Unlock()
			c.evictLRUUntilFits(ctx, newMemory)
			entry.mu.Lock()

			// Re-check after eviction (someone else might have created the blocks)
			newMemory = c.countNewBlockMemory(entry, chunkIdx, startBlock, endBlock)

			// Check if we have enough space after eviction.
			if c.totalSize.Load()+newMemory > c.maxSize {
				entry.mu.Unlock()
				return ErrCacheFull
			}
		}
	}

	// Backpressure on pending data (applies even when maxSize=0).
	// This prevents OOM when uploads can't keep up with writes.
	// Skip for downloaded data - it's already in block store, not pending.
	if !opts.isDownloaded {
		maxPending := c.maxPendingSize
		if maxPending == 0 {
			maxPending = DefaultMaxPendingSize
		}
		if newMemory > 0 && c.pendingSize.Load()+newMemory > maxPending {
			entry.mu.Unlock()
			return ErrCacheFull
		}
	}

	defer entry.mu.Unlock()

	// Update LRU access time
	c.touchFile(entry)

	// Write data to each block buffer it spans
	for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
		if err := c.writeToBlock(ctx, entry, payloadID, chunkIdx, blockIdx, data, offset, opts); err != nil {
			return err
		}
	}

	return nil
}

// countNewBlockMemory calculates memory needed for new blocks in the given range.
func (c *Cache) countNewBlockMemory(entry *fileEntry, chunkIdx, startBlock, endBlock uint32) uint64 {
	var newBlockCount uint64
	for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
		if !c.blockExists(entry, chunkIdx, blockIdx) {
			newBlockCount++
		}
	}
	return newBlockCount * BlockSize
}

// writeToBlock writes data to a single block buffer.
// Caller must hold entry.mu.Lock().
func (c *Cache) writeToBlock(ctx context.Context, entry *fileEntry, payloadID string, chunkIdx, blockIdx uint32, data []byte, offset uint32, opts writeOptions) error {
	// Get or create block buffer
	blk, isNew := c.getOrCreateBlock(entry, chunkIdx, blockIdx)

	// For normal writes (not downloaded), handle state transitions
	if !opts.isDownloaded {
		// Cannot write to a block that is currently being uploaded.
		if !isNew && blk.state == BlockStateUploading {
			return ErrCacheFull
		}

		// Handle write to ReadyForUpload block - cancel pending upload and revert to Pending.
		if !isNew && blk.state == BlockStateReadyForUpload {
			if blk.uploadCancel != nil {
				blk.uploadCancel()
				blk.uploadCancel = nil
			}
			blk.state = BlockStatePending
			blk.hash = [32]byte{} // Invalidate hash - will be recomputed on completion
		}
	}

	// Track memory and set initial state for new block buffers
	if isNew {
		c.totalSize.Add(BlockSize)
		if opts.isDownloaded {
			// Downloaded blocks start as Uploaded (evictable)
			blk.state = BlockStateUploaded
		} else {
			// Normal writes start as Pending
			c.pendingSize.Add(BlockSize)
		}
	}

	// Calculate offsets within this block
	blockStart := blockIdx * BlockSize
	blockEnd := blockStart + BlockSize

	// Calculate overlap with write range
	writeStart := max(offset, blockStart)
	writeEnd := min(offset+uint32(len(data)), blockEnd)

	// Calculate positions
	offsetInBlock := writeStart - blockStart
	dataStart := writeStart - offset
	dataEnd := writeEnd - offset
	writeLen := dataEnd - dataStart

	// Copy data directly to block buffer
	copy(blk.data[offsetInBlock:], data[dataStart:dataEnd])

	// Update coverage bitmap
	markCoverage(blk.coverage, offsetInBlock, writeLen)

	// Update block data size
	if end := offsetInBlock + writeLen; end > blk.dataSize {
		blk.dataSize = end
	}

	// For normal writes, mark block as dirty if it was uploaded (re-dirty)
	if !opts.isDownloaded && blk.state == BlockStateUploaded {
		blk.state = BlockStatePending
		c.pendingSize.Add(BlockSize) // Re-dirtied block becomes pending again
	}

	// Persist to WAL if enabled (only for normal writes)
	if !opts.isDownloaded && c.persister != nil {
		walEntry := &wal.BlockWriteEntry{
			PayloadID:     payloadID,
			ChunkIdx:      chunkIdx,
			BlockIdx:      blockIdx,
			OffsetInBlock: offsetInBlock,
			Data:          data[dataStart:dataEnd],
		}
		// Release lock during WAL write to avoid deadlock
		entry.mu.Unlock()
		err := c.persister.AppendBlockWrite(walEntry)
		entry.mu.Lock()
		if err != nil {
			return err
		}
	}

	return nil
}
