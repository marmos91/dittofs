// Package chunk defines constants and helpers for chunk-level file segmentation.
//
// The Chunk/Slice/Block model is DittoFS's approach to efficient file storage:
//
//   - Chunk: 64MB segment of a file (organizational unit)
//   - Slice: Contiguous write within a chunk (may overlap, newest-wins)
//   - Block: 4MB storage unit (what gets written to S3/filesystem)
//
// This package handles chunk-level operations. See pkg/payload/block for
// block-level operations.
//
// This model avoids read-modify-write for overwrites:
//   - Write 1KB to 100GB file → create 1KB slice, NOT re-upload 100GB
//   - Overlapping slices resolved at read time (newest wins)
//   - Background compaction merges slices when needed
package chunk

// ============================================================================
// Size Constants
// ============================================================================

const (
	// Size is the size of a chunk in bytes (64MB).
	// Files are divided into chunks for metadata organization and lazy loading.
	Size = 64 * 1024 * 1024

	// DefaultMaxSlicesPerChunk triggers compaction when exceeded.
	DefaultMaxSlicesPerChunk = 16
)

// ============================================================================
// Chunk Calculations
// ============================================================================

// IndexForOffset calculates the chunk index for a file offset.
//
// Example:
//
//	IndexForOffset(0)         → 0 (first chunk)
//	IndexForOffset(64*1024*1024) → 1 (second chunk)
func IndexForOffset(offset uint64) uint32 {
	return uint32(offset / Size)
}

// OffsetInChunk calculates the offset within a chunk.
//
// Example:
//
//	OffsetInChunk(1000)       → 1000 (within first chunk)
//	OffsetInChunk(64*1024*1024 + 1000) → 1000 (within second chunk)
func OffsetInChunk(offset uint64) uint32 {
	return uint32(offset % Size)
}

// Range calculates the range of chunks that a byte range spans.
// Returns startChunk and endChunk (inclusive).
//
// Example:
//
//	Range(0, 1000)            → (0, 0) single chunk
//	Range(0, 64*1024*1024+1)  → (0, 1) spans two chunks
func Range(offset, length uint64) (startChunk, endChunk uint32) {
	if length == 0 {
		return IndexForOffset(offset), IndexForOffset(offset)
	}
	startChunk = IndexForOffset(offset)
	endChunk = IndexForOffset(offset + length - 1)
	return startChunk, endChunk
}

// ============================================================================
// Range Helpers
// ============================================================================

// Bounds returns the file-level byte range for a chunk index.
// Returns (startOffset, endOffset) where endOffset is exclusive.
//
// Example:
//
//	Bounds(0) → (0, 67108864)
//	Bounds(1) → (67108864, 134217728)
func Bounds(chunkIdx uint32) (start, end uint64) {
	start = uint64(chunkIdx) * Size
	end = start + Size
	return start, end
}

// ClipToChunk clips a file-level range to chunk boundaries.
// Returns the portion of the range that falls within the specified chunk.
//
// Parameters:
//   - chunkIdx: The chunk to clip to
//   - fileOffset: Start offset in file coordinates
//   - length: Length of the range
//
// Returns:
//   - offsetInChunk: Start offset within the chunk
//   - clippedLength: Length of the clipped range (may be 0 if no overlap)
func ClipToChunk(chunkIdx uint32, fileOffset, length uint64) (offsetInChunk, clippedLength uint32) {
	chunkStart, chunkEnd := Bounds(chunkIdx)

	// Range ends before chunk starts
	if fileOffset+length <= chunkStart {
		return 0, 0
	}
	// Range starts after chunk ends
	if fileOffset >= chunkEnd {
		return 0, 0
	}

	// Calculate overlap
	rangeStart := max(fileOffset, chunkStart)
	rangeEnd := min(fileOffset+length, chunkEnd)

	offsetInChunk = uint32(rangeStart - chunkStart)
	clippedLength = uint32(rangeEnd - rangeStart)
	return offsetInChunk, clippedLength
}

// ============================================================================
// Chunk Slice Iterator
// ============================================================================

// Slice represents a portion of a byte range within a single chunk.
// Used by the Slices iterator to break a file-level range into per-chunk pieces.
type Slice struct {
	// ChunkIndex is the chunk this slice belongs to.
	ChunkIndex uint32

	// Offset is the byte offset within the chunk (0 to Size-1).
	Offset uint32

	// Length is the size of this slice in bytes.
	Length uint32

	// BufOffset is the offset into the caller's buffer where this slice's data
	// should be read from or written to.
	BufOffset int
}

// Slices returns an iterator over chunk slices for a file-level byte range.
// Each yielded Slice represents the portion of the range within a single chunk.
//
// This eliminates the common pattern of:
//
//	startChunk, endChunk := chunk.Range(offset, length)
//	for chunkIdx := startChunk; chunkIdx <= endChunk; chunkIdx++ {
//	    offsetInChunk, len := chunk.ClipToChunk(chunkIdx, offset+processed, remaining)
//	    // ...
//	}
//
// Usage:
//
//	for slice := range chunk.Slices(offset, uint64(len(buf))) {
//	    data, _, _ := cache.ReadSlice(ctx, handle, slice.ChunkIndex, slice.Offset, slice.Length)
//	    copy(buf[slice.BufOffset:], data)
//	}
func Slices(fileOffset, length uint64) func(yield func(Slice) bool) {
	return func(yield func(Slice) bool) {
		if length == 0 {
			return
		}

		startChunk, endChunk := Range(fileOffset, length)
		bufOffset := 0

		for chunkIdx := startChunk; chunkIdx <= endChunk; chunkIdx++ {
			offsetInChunk, sliceLen := ClipToChunk(chunkIdx, fileOffset+uint64(bufOffset), length-uint64(bufOffset))
			if sliceLen == 0 {
				continue
			}

			slice := Slice{
				ChunkIndex: chunkIdx,
				Offset:     offsetInChunk,
				Length:     sliceLen,
				BufOffset:  bufOffset,
			}

			if !yield(slice) {
				return
			}

			bufOffset += int(sliceLen)
		}
	}
}
