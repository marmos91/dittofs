package cache

import (
	"context"
)

// ============================================================================
// Read Operations
// ============================================================================

// ReadSlice reads data from cache with slice merging (newest-wins).
// Data is written directly into dest buffer. Returns true if data was found.
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
	c.mergeSlicesForRead(chunk.slices, offset, length, dest)

	return true, nil
}

// mergeSlicesForRead implements the newest-wins slice merge algorithm.
// Writes directly into dest buffer. Returns true if entire range is covered.
func (c *Cache) mergeSlicesForRead(slices []Slice, offset, length uint32, dest []byte) bool {
	covered := make([]bool, length)
	coveredCount := uint32(0)

	requestEnd := offset + length

	for _, slice := range slices {
		if coveredCount >= length {
			break
		}

		sliceEnd := slice.Offset + slice.Length

		if slice.Offset >= requestEnd || sliceEnd <= offset {
			continue
		}

		overlapStart := max(offset, slice.Offset)
		overlapEnd := min(requestEnd, sliceEnd)

		for i := overlapStart; i < overlapEnd; i++ {
			resultIdx := i - offset
			if !covered[resultIdx] {
				sliceIdx := i - slice.Offset
				dest[resultIdx] = slice.Data[sliceIdx]
				covered[resultIdx] = true
				coveredCount++
			}
		}
	}

	return coveredCount == length
}

// IsRangeCovered checks if a byte range is fully covered by cached slices.
// This is used by the flusher to determine if a block is ready for upload.
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

	// Check coverage without allocating result buffer
	coveredCount := uint32(0)
	requestEnd := offset + length

	for _, slice := range chunk.slices {
		if coveredCount >= length {
			break
		}

		sliceEnd := slice.Offset + slice.Length

		if slice.Offset >= requestEnd || sliceEnd <= offset {
			continue
		}

		overlapStart := max(offset, slice.Offset)
		overlapEnd := min(requestEnd, sliceEnd)
		coveredCount += overlapEnd - overlapStart
	}

	return coveredCount >= length, nil
}
