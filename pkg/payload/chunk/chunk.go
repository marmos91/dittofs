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
// Block Range Iterator
// ============================================================================

// BlockSize is the size of a block in bytes (4MB).
// Blocks are the unit of storage, hash computation, and deduplication.
const BlockSize = 4 * 1024 * 1024

// BlocksPerChunk is the number of blocks in a full chunk (16).
const BlocksPerChunk = Size / BlockSize

// BlockRange represents a contiguous write range within a single 4MB block.
// Used to split writes across block boundaries for completion tracking,
// hash computation, and deduplication.
type BlockRange struct {
	// ChunkIndex is which 64MB chunk (0, 1, 2, ...).
	ChunkIndex uint32

	// BlockIndex is which 4MB block within the chunk (0 to BlocksPerChunk-1).
	BlockIndex uint32

	// Offset is the byte offset within the block (0 to BlockSize-1).
	Offset uint32

	// Length is the length of data in this block range.
	Length uint32

	// BufOffset is the offset into the caller's buffer where this range's data
	// should be read from or written to.
	BufOffset int
}

// BlockRanges returns an iterator over write ranges at 4MB block boundaries.
// Splits writes that cross block boundaries, providing chunk+block coordinates
// for each segment. This is the primary iterator for write operations.
//
// Example: A 10MB write starting at offset 2MB would yield:
//   - BlockRange{ChunkIndex: 0, BlockIndex: 0, Offset: 2MB, Length: 2MB, BufOffset: 0}
//   - BlockRange{ChunkIndex: 0, BlockIndex: 1, Offset: 0, Length: 4MB, BufOffset: 2MB}
//   - BlockRange{ChunkIndex: 0, BlockIndex: 2, Offset: 0, Length: 4MB, BufOffset: 6MB}
//
// Usage:
//
//	for br := range chunk.BlockRanges(offset, len(data)) {
//	    segment := data[br.BufOffset : br.BufOffset+int(br.Length)]
//	    cache.WriteAt(ctx, payloadID, br.ChunkIndex, segment, br.Offset + br.BlockIndex*BlockSize)
//	}
func BlockRanges(fileOffset uint64, length int) func(yield func(BlockRange) bool) {
	return func(yield func(BlockRange) bool) {
		if length <= 0 {
			return
		}

		remaining := uint64(length)
		currentOffset := fileOffset
		bufOffset := 0

		for remaining > 0 {
			// Calculate chunk coordinates
			chunkIdx := uint32(currentOffset / Size)
			offsetInChunk := currentOffset % Size

			// Calculate block coordinates within chunk
			blockIdx := uint32(offsetInChunk / BlockSize)
			offsetInBlock := uint32(offsetInChunk % BlockSize)

			// How much can we write in this block?
			spaceInBlock := uint64(BlockSize - offsetInBlock)
			writeLen := min(remaining, spaceInBlock)

			br := BlockRange{
				ChunkIndex: chunkIdx,
				BlockIndex: blockIdx,
				Offset:     offsetInBlock,
				Length:     uint32(writeLen),
				BufOffset:  bufOffset,
			}

			if !yield(br) {
				return
			}

			currentOffset += writeLen
			bufOffset += int(writeLen)
			remaining -= writeLen
		}
	}
}

// ChunkOffsetForBlock returns the chunk-level offset for a block index.
// This is the offset within the chunk where the block starts.
func ChunkOffsetForBlock(blockIdx uint32) uint32 {
	return blockIdx * BlockSize
}
