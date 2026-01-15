package cache

import (
	"context"
)

// ============================================================================
// Read Operations
// ============================================================================

// ReadSlice reads data from the cache into the provided buffer.
//
// This is the primary read path. Data is read using newest-wins semantics:
// when multiple slices overlap, the most recently written slice takes precedence.
// This ensures read-your-writes consistency even with overlapping writes.
//
// The data is written directly into the dest buffer to avoid allocations.
// The buffer must be at least 'length' bytes.
//
// Parameters:
//   - fileHandle: Unique identifier for the file (from metadata store)
//   - chunkIdx: Which 64MB chunk to read from
//   - offset: Byte offset within the chunk to start reading
//   - length: Number of bytes to read
//   - dest: Buffer to write data into (must be >= length bytes)
//
// Returns:
//   - found: true if any data was found for this file/chunk
//   - error: context errors or ErrCacheClosed
//
// Note: If found=true but the range isn't fully covered by slices, the uncovered
// portions of dest will contain zeros. Use IsRangeCovered to check coverage.
func (c *Cache) ReadSlice(ctx context.Context, fileHandle string, chunkIdx uint32, offset, length uint32, dest []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return false, ErrCacheClosed
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(fileHandle)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	chunk, exists := entry.chunks[chunkIdx]
	if !exists || len(chunk.slices) == 0 {
		return false, nil
	}

	// Merge slices using newest-wins algorithm directly into dest
	mergeSlicesForRead(chunk.slices, offset, length, dest)

	return true, nil
}

// mergeSlicesForRead implements the newest-wins slice merge algorithm.
//
// Slices are assumed to be ordered newest-first (as maintained by WriteSlice).
// For each byte position in the requested range, the first slice that covers
// it wins. This naturally implements read-your-writes semantics.
//
// Algorithm:
//  1. Create a coverage bitmap for the requested range
//  2. Iterate slices newest-first
//  3. For each slice, copy bytes that aren't already covered
//  4. Stop early if entire range is covered
//
// Writes directly into dest buffer to avoid allocations.
// Returns true if the entire requested range is covered by slices.
func mergeSlicesForRead(slices []Slice, offset, length uint32, dest []byte) bool {
	return walkSliceCoverage(slices, offset, length, func(resultIdx, sliceIdx uint32, slice *Slice) {
		dest[resultIdx] = slice.Data[sliceIdx]
	})
}

// walkSliceCoverage iterates over slice coverage using newest-wins semantics.
//
// For each uncovered byte in the range [offset, offset+length), calls the visitor
// with the result index, slice index, and slice. Uses a bitmap to track coverage
// and avoid visiting the same byte twice.
//
// Returns true if the entire range is covered by slices.
func walkSliceCoverage(slices []Slice, offset, length uint32, visit func(resultIdx, sliceIdx uint32, slice *Slice)) bool {
	covered := make([]bool, length)
	coveredCount := uint32(0)
	requestEnd := offset + length

	for i := range slices {
		if coveredCount >= length {
			break
		}

		slice := &slices[i]
		sliceEnd := slice.Offset + slice.Length

		// Skip non-overlapping slices
		if slice.Offset >= requestEnd || sliceEnd <= offset {
			continue
		}

		overlapStart := max(offset, slice.Offset)
		overlapEnd := min(requestEnd, sliceEnd)

		for j := overlapStart; j < overlapEnd; j++ {
			resultIdx := j - offset
			if !covered[resultIdx] {
				if visit != nil {
					visit(resultIdx, j-slice.Offset, slice)
				}
				covered[resultIdx] = true
				coveredCount++
			}
		}
	}

	return coveredCount == length
}

// IsRangeCovered checks if a byte range is fully covered by cached slices.
//
// This is used by the TransferManager to determine if a block can be uploaded.
// A block is ready for upload when all its bytes are present in the cache.
//
// Uses the same bitmap-based coverage tracking as ReadSlice to correctly handle
// overlapping slices. Without proper tracking, overlapping slices could cause
// false positives (reporting coverage when gaps exist).
//
// Parameters:
//   - fileHandle: Unique identifier for the file
//   - chunkIdx: Which 64MB chunk to check
//   - offset: Start of range within chunk
//   - length: Size of range to check
//
// Returns:
//   - covered: true if every byte in [offset, offset+length) is covered
//   - error: context errors or ErrCacheClosed
func (c *Cache) IsRangeCovered(ctx context.Context, fileHandle string, chunkIdx uint32, offset, length uint32) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return false, ErrCacheClosed
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(fileHandle)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	chunk, exists := entry.chunks[chunkIdx]
	if !exists || len(chunk.slices) == 0 {
		return false, nil
	}

	// Use shared coverage walker with nil visitor (just check coverage, don't copy data)
	return walkSliceCoverage(chunk.slices, offset, length, nil), nil
}
