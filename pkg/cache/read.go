package cache

import (
	"context"
	"math/bits"
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
// Note: If found=true but the range isn't fully covered by slices, the uncovered
// portions of dest will contain zeros. Use IsRangeCovered to check coverage.
func (c *Cache) ReadSlice(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32, dest []byte) (bool, error) {
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
// Optimized for the common case where the newest slice covers the entire range
// (sequential writes extend one slice). Falls back to bitmap-based merge for
// complex overlapping slice scenarios.
//
// Writes directly into dest buffer to avoid allocations.
// Returns true if the entire requested range is covered by slices.
func mergeSlicesForRead(slices []Slice, offset, length uint32, dest []byte) bool {
	if length == 0 {
		return true
	}
	if len(slices) == 0 {
		return false
	}

	requestEnd := offset + length

	// Fast path: newest slice covers entire range
	// This is the common case for sequential writes (cache extends one slice)
	// Only check first slice since it's the newest and takes full precedence
	newest := &slices[0]
	if newest.Offset <= offset && newest.Offset+newest.Length >= requestEnd {
		// Newest slice covers range - direct copy, no bitmap needed
		srcStart := offset - newest.Offset
		copy(dest, newest.Data[srcStart:srcStart+length])
		return true
	}

	// Slow path: multiple overlapping slices, use block-copy merge
	return mergeSlicesBlockCopy(slices, offset, length, dest)
}

// mergeSlicesBlockCopy merges slices into dest using block copies.
//
// This is optimized for performance: instead of copying byte-by-byte,
// it finds contiguous uncovered ranges and copies them in bulk.
// Uses a bitset for tracking coverage with word-level operations.
//
// Returns true if the entire requested range is covered by slices.
func mergeSlicesBlockCopy(slices []Slice, offset, length uint32, dest []byte) bool {
	if length == 0 {
		return true
	}
	if len(slices) == 0 {
		return false
	}

	// Use bitset for coverage tracking (8x less memory than []bool)
	// Each uint64 tracks 64 bytes of coverage
	numWords := (length + 63) / 64
	covered := make([]uint64, numWords)
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

		// Process range using word-aligned operations where possible
		j := overlapStart
		for j < overlapEnd {
			resultIdx := j - offset
			wordIdx := resultIdx / 64
			bitIdx := resultIdx % 64

			// Check if this byte is already covered
			if covered[wordIdx]&(1<<bitIdx) != 0 {
				// Skip to next uncovered byte using bit tricks
				j = skipCoveredBytes(covered, resultIdx, length) + offset
				continue
			}

			// Find contiguous uncovered range using word operations
			rangeStart := j
			rangeEnd := findCoveredByte(covered, resultIdx, min(overlapEnd-offset, length)) + offset
			if rangeEnd > overlapEnd {
				rangeEnd = overlapEnd
			}
			j = rangeEnd

			// Block copy this range
			srcStart := rangeStart - slice.Offset
			dstStart := rangeStart - offset
			copyLen := rangeEnd - rangeStart

			copy(dest[dstStart:dstStart+copyLen], slice.Data[srcStart:srcStart+copyLen])

			// Mark range as covered using word operations
			markRangeCovered(covered, dstStart, copyLen)
			coveredCount += copyLen
		}
	}

	return coveredCount == length
}

// skipCoveredBytes finds the next uncovered byte starting from pos.
// Uses word-level operations to skip 64 bytes at a time.
func skipCoveredBytes(covered []uint64, pos, length uint32) uint32 {
	wordIdx := pos / 64
	bitIdx := pos % 64

	// Check remaining bits in current word
	word := covered[wordIdx] >> bitIdx
	if word != ^uint64(0)>>bitIdx {
		// There's an uncovered bit in this word
		inverted := ^word
		nextUncovered := uint32(bits.TrailingZeros64(inverted))
		return pos + nextUncovered
	}

	// Skip full words
	wordIdx++
	numWords := uint32(len(covered))
	for wordIdx < numWords && covered[wordIdx] == ^uint64(0) {
		wordIdx++
	}

	if wordIdx >= numWords {
		return length // All remaining bytes are covered
	}

	// Find first uncovered bit in this word
	inverted := ^covered[wordIdx]
	nextUncovered := uint32(bits.TrailingZeros64(inverted))
	result := wordIdx*64 + nextUncovered
	if result > length {
		return length
	}
	return result
}

// findCoveredByte finds the next covered byte starting from pos.
// Returns length if no covered byte is found before length.
// Uses word-level operations to scan 64 bytes at a time.
func findCoveredByte(covered []uint64, pos, length uint32) uint32 {
	wordIdx := pos / 64
	bitIdx := pos % 64

	// Check remaining bits in current word
	word := covered[wordIdx] >> bitIdx
	if word != 0 {
		// There's a covered bit in this word
		nextCovered := uint32(bits.TrailingZeros64(word))
		result := pos + nextCovered
		if result > length {
			return length
		}
		return result
	}

	// Skip full words
	wordIdx++
	numWords := uint32(len(covered))
	for wordIdx < numWords && covered[wordIdx] == 0 {
		wordIdx++
	}

	if wordIdx >= numWords {
		return length // No covered bytes found
	}

	// Find first covered bit in this word
	nextCovered := uint32(bits.TrailingZeros64(covered[wordIdx]))
	result := wordIdx*64 + nextCovered
	if result > length {
		return length
	}
	return result
}

// markRangeCovered sets bits [start, start+length) in the bitset.
// Uses word-level operations for efficiency.
func markRangeCovered(covered []uint64, start, length uint32) {
	end := start + length

	// Handle start partial word
	startWord := start / 64
	startBit := start % 64
	endWord := (end - 1) / 64
	endBit := (end-1)%64 + 1

	if startWord == endWord {
		// All bits in same word
		mask := (uint64(1)<<(endBit-startBit) - 1) << startBit
		covered[startWord] |= mask
		return
	}

	// Start partial word
	if startBit > 0 {
		covered[startWord] |= ^uint64(0) << startBit
		startWord++
	}

	// Full words in the middle
	for w := startWord; w < endWord; w++ {
		covered[w] = ^uint64(0)
	}

	// End partial word
	if endBit > 0 {
		covered[endWord] |= (uint64(1) << endBit) - 1
	}
}

// isRangeCoveredFast checks if a range is fully covered by slices without allocating.
//
// Uses interval tracking instead of a byte bitmap. This is O(n*log(n)) where n is
// the number of overlapping slices, but uses O(1) memory.
//
// Algorithm:
//  1. Collect all slice intervals that overlap the requested range
//  2. Sort by start offset
//  3. Walk intervals, tracking the furthest covered byte
//  4. Return true if entire range is covered without gaps
func isRangeCoveredFast(slices []Slice, offset, length uint32) bool {
	if length == 0 {
		return true
	}

	requestEnd := offset + length

	// Fast path: single slice covers entire range
	// This is the common case for sequential writes (cache extends the same slice)
	for i := range slices {
		if slices[i].Offset <= offset && slices[i].Offset+slices[i].Length >= requestEnd {
			return true
		}
	}

	// Fast path: no slices
	if len(slices) == 0 {
		return false
	}

	// Collect overlapping intervals
	type interval struct {
		start, end uint32
	}
	intervals := make([]interval, 0, len(slices))

	for i := range slices {
		slice := &slices[i]
		sliceEnd := slice.Offset + slice.Length

		// Skip non-overlapping slices
		if slice.Offset >= requestEnd || sliceEnd <= offset {
			continue
		}

		// Clip to requested range
		start := max(offset, slice.Offset)
		end := min(requestEnd, sliceEnd)
		intervals = append(intervals, interval{start, end})
	}

	if len(intervals) == 0 {
		return false
	}

	// Sort by start offset (insertion sort for small n)
	for i := 1; i < len(intervals); i++ {
		for j := i; j > 0 && intervals[j].start < intervals[j-1].start; j-- {
			intervals[j], intervals[j-1] = intervals[j-1], intervals[j]
		}
	}

	// Walk intervals tracking coverage
	covered := offset
	for _, iv := range intervals {
		if iv.start > covered {
			// Gap found
			return false
		}
		if iv.end > covered {
			covered = iv.end
		}
	}

	return covered >= requestEnd
}

// IsRangeCovered checks if a byte range is fully covered by cached slices.
//
// This is used by the TransferManager to determine if a block can be uploaded.
// A block is ready for upload when all its bytes are present in the cache.
//
// Uses an optimized interval-based algorithm that doesn't allocate O(length)
// memory. For sequential writes, the common case (single slice covers range)
// is O(1).
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
	if !exists || len(chunk.slices) == 0 {
		return false, nil
	}

	// Use optimized coverage check that doesn't allocate O(length) memory
	return isRangeCoveredFast(chunk.slices, offset, length), nil
}
