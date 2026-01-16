package cache

import (
	"context"

	"github.com/marmos91/dittofs/pkg/payload/block"
)

// ============================================================================
// Read Operations
// ============================================================================

// Read reads data from the cache into the provided buffer.
//
// This is the primary read path. Data is read from block buffers, with
// uncovered regions (sparse file holes) zero-filled in the destination buffer.
//
// The data is written directly into the dest buffer to avoid allocations.
// The buffer must be at least 'length' bytes.
//
// Parameters:
//   - payloadID: Unique identifier for the file content
//   - chunkIdx: Which 64MB chunk to read from
//   - offset: Byte offset within the chunk to start reading
//   - length: Number of bytes to read
//   - dest: Buffer to write data into (must be >= length bytes)
//
// Returns:
//   - found: true if any data was found for this file/chunk
//   - error: context errors or ErrCacheClosed
//
// Note: Uncovered portions of dest will contain zeros (sparse file behavior).
// Use IsRangeCovered to check full coverage.
func (c *Cache) Read(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32, dest []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return false, ErrCacheClosed
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(payloadID)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	chunk, exists := entry.chunks[chunkIdx]
	if !exists || len(chunk.blocks) == 0 {
		return false, nil
	}

	// Calculate which blocks this read spans
	startBlock := block.IndexForOffset(offset)
	endBlock := block.IndexForOffset(offset + length - 1)

	foundAny := false

	// Read from each block buffer
	for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
		blk, exists := chunk.blocks[blockIdx]
		if !exists || blk.data == nil {
			// Block doesn't exist or was evicted - zeros remain in dest (sparse file)
			continue
		}

		foundAny = true

		// Calculate offsets
		blockStart := blockIdx * BlockSize
		blockEnd := blockStart + BlockSize

		// Calculate overlap with read range
		readStart := max(offset, blockStart)
		readEnd := min(offset+length, blockEnd)

		// Calculate positions
		offsetInBlock := readStart - blockStart
		destStart := readStart - offset
		readLen := readEnd - readStart

		// Copy data from block buffer to dest
		copy(dest[destStart:], blk.data[offsetInBlock:offsetInBlock+readLen])
	}

	return foundAny, nil
}

// IsRangeCovered checks if a byte range is fully covered by cached data.
//
// This is used by the TransferManager to determine if a block can be uploaded.
// A block is ready for upload when all its bytes are present in the cache.
//
// Parameters:
//   - payloadID: Unique identifier for the file content
//   - chunkIdx: Which 64MB chunk to check
//   - offset: Start of range within chunk
//   - length: Size of range to check
//
// Returns:
//   - covered: true if every byte in [offset, offset+length) is covered
//   - error: context errors or ErrCacheClosed
func (c *Cache) IsRangeCovered(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return false, ErrCacheClosed
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(payloadID)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	chunk, exists := entry.chunks[chunkIdx]
	if !exists || len(chunk.blocks) == 0 {
		return false, nil
	}

	// Calculate which blocks this range spans
	startBlock := block.IndexForOffset(offset)
	endBlock := block.IndexForOffset(offset + length - 1)

	// Check each block
	for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
		blk, exists := chunk.blocks[blockIdx]
		if !exists || blk.data == nil {
			return false, nil
		}

		// Calculate overlap with check range
		blockStart := blockIdx * BlockSize
		blockEnd := blockStart + BlockSize

		checkStart := max(offset, blockStart)
		checkEnd := min(offset+length, blockEnd)

		// Calculate positions within block
		offsetInBlock := checkStart - blockStart
		checkLen := checkEnd - checkStart

		// Check coverage bitmap
		if !isRangeCovered(blk.coverage, offsetInBlock, checkLen) {
			return false, nil
		}
	}

	return true, nil
}

// IsBlockFullyCovered checks if an entire 4MB block is fully covered.
//
// This is used by the TransferManager to determine if a block can be uploaded
// for eager upload (before NFS COMMIT).
//
// Parameters:
//   - payloadID: Unique identifier for the file content
//   - chunkIdx: Which 64MB chunk to check
//   - blockIdx: Which 4MB block within the chunk
//
// Returns:
//   - covered: true if all 4MB of the block are covered
//   - error: context errors or ErrCacheClosed
func (c *Cache) IsBlockFullyCovered(ctx context.Context, payloadID string, chunkIdx, blockIdx uint32) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return false, ErrCacheClosed
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(payloadID)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	chunk, exists := entry.chunks[chunkIdx]
	if !exists {
		return false, nil
	}

	blk, exists := chunk.blocks[blockIdx]
	if !exists || blk.data == nil {
		return false, nil
	}

	return isFullyCovered(blk.coverage), nil
}
