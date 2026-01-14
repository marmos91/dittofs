// Package block defines constants and helpers for block-level storage operations.
//
// Blocks are the physical storage units in DittoFS - each block becomes a single
// object in the block store (S3, filesystem). The 4MB default size balances:
//   - S3 PUT efficiency (larger objects = better throughput)
//   - Memory usage (reasonable buffer size)
//   - Latency (partial blocks on COMMIT are manageable)
package block

// ============================================================================
// Size Constants
// ============================================================================

const (
	// Size is the default block size for storage (4MB).
	// Each block becomes a single object in the block store (S3, filesystem).
	Size = 4 * 1024 * 1024

	// MinSize is the minimum allowed block size (1MB).
	MinSize = 1 * 1024 * 1024

	// MaxSize is the maximum allowed block size (16MB).
	MaxSize = 16 * 1024 * 1024
)

// ============================================================================
// Block Calculations
// ============================================================================

// IndexForOffset calculates the block index within a chunk for an offset.
//
// Example:
//
//	IndexForOffset(0)           → 0 (first block)
//	IndexForOffset(4*1024*1024) → 1 (second block)
func IndexForOffset(offsetInChunk uint32) uint32 {
	return offsetInChunk / Size
}

// OffsetInBlock calculates the offset within a block.
//
// Example:
//
//	OffsetInBlock(1000)         → 1000 (within first block)
//	OffsetInBlock(4*1024*1024 + 1000) → 1000 (within second block)
func OffsetInBlock(offsetInChunk uint32) uint32 {
	return offsetInChunk % Size
}

// Range calculates the range of blocks that a byte range spans within a chunk.
// Returns startBlock and endBlock (inclusive).
//
// Example:
//
//	Range(0, 1000)              → (0, 0) single block
//	Range(0, 4*1024*1024+1)     → (0, 1) spans two blocks
func Range(offsetInChunk, length uint32) (startBlock, endBlock uint32) {
	if length == 0 {
		return IndexForOffset(offsetInChunk), IndexForOffset(offsetInChunk)
	}
	startBlock = IndexForOffset(offsetInChunk)
	endBlock = IndexForOffset(offsetInChunk + length - 1)
	return startBlock, endBlock
}

// Bounds returns the chunk-level byte range for a block index.
// Returns (startOffset, endOffset) where endOffset is exclusive.
//
// Example:
//
//	Bounds(0) → (0, 4194304)
//	Bounds(1) → (4194304, 8388608)
func Bounds(blockIdx uint32) (start, end uint32) {
	start = blockIdx * Size
	end = start + Size
	return start, end
}

// PerChunk returns the number of blocks in a full chunk.
// With default sizes (64MB chunk, 4MB block), this returns 16.
func PerChunk(chunkSize uint32) uint32 {
	return chunkSize / Size
}
