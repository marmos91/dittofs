package cache

import (
	"context"

	"github.com/marmos91/dittofs/pkg/cache/wal"
	"github.com/marmos91/dittofs/pkg/payload/block"
)

// ============================================================================
// Write Operations
// ============================================================================

// Write writes data to the cache at the specified chunk and offset.
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
func (c *Cache) Write(ctx context.Context, payloadID string, chunkIdx uint32, data []byte, offset uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return ErrCacheClosed
	}
	c.globalMu.RUnlock()

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
	// We track actual memory allocation (BlockSize per block), not data written.
	var newBlockCount uint64
	for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
		if !c.blockExists(entry, chunkIdx, blockIdx) {
			newBlockCount++
		}
	}
	newMemory := newBlockCount * BlockSize

	// Enforce maxSize by evicting LRU uploaded blocks if needed.
	// If eviction can't free enough space (all data is pending), return ErrCacheFull
	// to provide backpressure and prevent OOM conditions.
	if c.maxSize > 0 && newMemory > 0 {
		if c.totalSize.Load()+newMemory > c.maxSize {
			// Release lock to evict (eviction needs to lock other entries)
			entry.mu.Unlock()
			c.evictLRUUntilFits(ctx, newMemory)
			entry.mu.Lock()

			// Re-check after eviction (someone else might have created the blocks)
			newBlockCount = 0
			for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
				if !c.blockExists(entry, chunkIdx, blockIdx) {
					newBlockCount++
				}
			}
			newMemory = newBlockCount * BlockSize

			// Check if we have enough space after eviction.
			// If not, cache is full of pending data that can't be evicted.
			if c.totalSize.Load()+newMemory > c.maxSize {
				entry.mu.Unlock()
				return ErrCacheFull
			}
		}
	}

	defer entry.mu.Unlock()

	// Update LRU access time
	c.touchFile(entry)

	// Write data to each block buffer it spans
	for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
		// Get or create block buffer
		blk, isNew := c.getOrCreateBlock(entry, chunkIdx, blockIdx)

		// Track memory for new block buffers
		if isNew {
			c.totalSize.Add(BlockSize)
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

		// Mark block as dirty if it was uploaded
		if blk.state == BlockStateUploaded {
			blk.state = BlockStatePending
		}

		// Persist to WAL if enabled
		if c.persister != nil {
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
	}

	return nil
}
