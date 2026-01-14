// Package chunk defines constants and helpers for the Chunk/Slice/Block storage model.
//
// The Chunk/Slice/Block model is DittoFS's approach to efficient file storage:
//
//   - Chunk: 64MB segment of a file (organizational unit)
//   - Slice: Contiguous write within a chunk (may overlap, newest-wins)
//   - Block: 4MB storage unit (what gets written to S3/filesystem)
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
	// ChunkSize is the size of a chunk in bytes (64MB).
	// Files are divided into chunks for metadata organization and lazy loading.
	ChunkSize = 64 * 1024 * 1024

	// BlockSize is the default block size for storage (4MB).
	// Each block becomes a single object in the block store (S3, filesystem).
	// This size balances S3 PUT efficiency with memory usage and latency.
	BlockSize = 4 * 1024 * 1024

	// MinBlockSize is the minimum allowed block size (1MB).
	MinBlockSize = 1 * 1024 * 1024

	// MaxBlockSize is the maximum allowed block size (16MB).
	MaxBlockSize = 16 * 1024 * 1024

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
	return uint32(offset / ChunkSize)
}

// OffsetInChunk calculates the offset within a chunk.
//
// Example:
//
//	OffsetInChunk(1000)       → 1000 (within first chunk)
//	OffsetInChunk(64*1024*1024 + 1000) → 1000 (within second chunk)
func OffsetInChunk(offset uint64) uint32 {
	return uint32(offset % ChunkSize)
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
// Block Calculations
// ============================================================================

// BlockIndexForOffset calculates the block index within a chunk for an offset.
//
// Example:
//
//	BlockIndexForOffset(0)         → 0 (first block)
//	BlockIndexForOffset(4*1024*1024) → 1 (second block)
func BlockIndexForOffset(offsetInChunk uint32) uint32 {
	return offsetInChunk / BlockSize
}

// OffsetInBlock calculates the offset within a block.
func OffsetInBlock(offsetInChunk uint32) uint32 {
	return offsetInChunk % BlockSize
}

// BlockRange calculates the range of blocks that a byte range spans within a chunk.
// Returns startBlock and endBlock (inclusive).
func BlockRange(offsetInChunk, length uint32) (startBlock, endBlock uint32) {
	if length == 0 {
		return BlockIndexForOffset(offsetInChunk), BlockIndexForOffset(offsetInChunk)
	}
	startBlock = BlockIndexForOffset(offsetInChunk)
	endBlock = BlockIndexForOffset(offsetInChunk + length - 1)
	return startBlock, endBlock
}

// BlocksPerChunk returns the number of blocks in a full chunk.
func BlocksPerChunk() uint32 {
	return ChunkSize / BlockSize
}

// ============================================================================
// Range Helpers
// ============================================================================

// ChunkBounds returns the file-level byte range for a chunk index.
// Returns (startOffset, endOffset) where endOffset is exclusive.
func ChunkBounds(chunkIdx uint32) (start, end uint64) {
	start = uint64(chunkIdx) * ChunkSize
	end = start + ChunkSize
	return start, end
}

// BlockBounds returns the chunk-level byte range for a block index.
// Returns (startOffset, endOffset) where endOffset is exclusive.
func BlockBounds(blockIdx uint32) (start, end uint32) {
	start = blockIdx * BlockSize
	end = start + BlockSize
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
	chunkStart, chunkEnd := ChunkBounds(chunkIdx)

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
