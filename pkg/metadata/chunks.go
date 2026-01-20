package metadata

import (
	"github.com/marmos91/dittofs/pkg/payload/block"
	"github.com/marmos91/dittofs/pkg/payload/chunk"
)

// ============================================================================
// Chunk/Block Constants (re-exported from chunk and block packages)
// ============================================================================

const (
	ChunkSize                = chunk.Size
	DefaultBlockSize         = block.Size
	MinBlockSize             = block.MinSize
	MaxBlockSize             = block.MaxSize
	DefaultMaxSlicesPerChunk = chunk.DefaultMaxSlicesPerChunk
)

// ============================================================================
// Helper Functions (delegating to chunk package)
// ============================================================================

// ChunkIndexForOffset calculates the chunk index for a file offset.
// Deprecated: Use chunk.IndexForOffset directly.
func ChunkIndexForOffset(offset uint64) uint32 {
	return chunk.IndexForOffset(offset)
}

// OffsetWithinChunk calculates the offset within a chunk.
// Deprecated: Use chunk.OffsetInChunk directly.
func OffsetWithinChunk(offset uint64) uint32 {
	return chunk.OffsetInChunk(offset)
}

// ChunkRange calculates the range of chunks that a byte range spans.
// Returns startChunk and endChunk (inclusive).
// Deprecated: Use chunk.Range directly.
func ChunkRange(offset, length uint64) (startChunk, endChunk uint32) {
	return chunk.Range(offset, length)
}
