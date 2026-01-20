// Package cache implements buffering for content stores.
package cache

import (
	"errors"
	"sync/atomic"

	"github.com/marmos91/dittofs/pkg/payload/block"
	"github.com/marmos91/dittofs/pkg/payload/chunk"
)

// Re-export chunk and block constants for backward compatibility.
// New code should import pkg/payload/chunk and pkg/payload/block directly.
const (
	ChunkSize      = chunk.Size // 64MB
	BlockSize      = block.Size // 4MB
	MinBlockSize   = block.MinSize
	MaxBlockSize   = block.MaxSize
	BlocksPerChunk = ChunkSize / BlockSize // 16 blocks per 64MB chunk
)

// Coverage bitmap constants.
// We track coverage at 64-byte granularity (1 bit per 64 bytes).
// For a 4MB block: 4MB / 64 = 65536 bits = 1024 uint64 words = 8KB bitmap
const (
	CoverageGranularity   = 64                                                    // bytes per coverage bit
	CoverageBitsPerWord   = 64                                                    // bits per uint64
	CoverageWordsPerBlock = BlockSize / CoverageGranularity / CoverageBitsPerWord // 1024
)

// Backpressure constants.
// MaxPendingSize limits pending (dirty) data to prevent OOM even when cache is unlimited.
const (
	DefaultMaxPendingSize = 512 * 1024 * 1024 // 512MB default limit for pending data
)

// ============================================================================
// Errors
// ============================================================================

var (
	// ErrCacheClosed is returned when operations are attempted on a closed cache.
	ErrCacheClosed = errors.New("cache is closed")

	// ErrBlockNotFound is returned when a requested block doesn't exist.
	ErrBlockNotFound = errors.New("block not found")

	// ErrFileNotInCache is returned when a file has no cached data.
	ErrFileNotInCache = errors.New("file not in cache")

	// ErrInvalidChunkIndex is returned for out-of-range chunk indices.
	ErrInvalidChunkIndex = errors.New("invalid chunk index")

	// ErrInvalidOffset is returned for invalid offsets.
	ErrInvalidOffset = errors.New("invalid offset")

	// ErrCacheFull is returned when the cache is full of pending data that
	// cannot be evicted. This provides backpressure to prevent OOM conditions.
	// The caller should flush data (NFS COMMIT) before retrying the write.
	ErrCacheFull = errors.New("cache full: pending data cannot be evicted")
)

// ============================================================================
// Atomic Helpers
// ============================================================================

// atomicSubtract subtracts n from the atomic value.
//
// Go's atomic.Uint64 doesn't have a Subtract method, only Add. To subtract,
// we use two's complement arithmetic: Add(^(n-1)) is equivalent to Add(-n)
// for unsigned types because ^(n-1) == -n in two's complement representation.
//
// Example: to subtract 5, we compute ^4 (bitwise NOT of 4):
//
//	^4 = 0xFFFFFFFFFFFFFFFB (in 64-bit)
//	When added to any value, this has the same effect as subtracting 5.
//
// This pattern is common in Go's standard library (see sync/atomic internals).
func atomicSubtract(a *atomic.Uint64, n uint64) {
	a.Add(^(n - 1))
}

// ============================================================================
// Block Buffer Types
// ============================================================================

// BlockState represents the state of a block buffer in the cache.
type BlockState int

const (
	// BlockStatePending indicates the block has unflushed data.
	// New writes arrive here, hash not yet computed.
	BlockStatePending BlockState = iota

	// BlockStateReadyForUpload indicates the block is complete and hash computed.
	// The block is queued for upload. If new writes arrive, cancel upload and
	// revert to Pending.
	BlockStateReadyForUpload

	// BlockStateUploading indicates the block is currently being uploaded.
	// The buffer has been detached for zero-copy upload.
	BlockStateUploading

	// BlockStateUploaded indicates the block has been uploaded to storage.
	// The buffer can be evicted if memory pressure requires it.
	BlockStateUploaded
)

// String returns the string representation of BlockState.
func (s BlockState) String() string {
	switch s {
	case BlockStatePending:
		return "Pending"
	case BlockStateReadyForUpload:
		return "ReadyForUpload"
	case BlockStateUploading:
		return "Uploading"
	case BlockStateUploaded:
		return "Uploaded"
	default:
		return "Unknown"
	}
}

// blockBuffer represents a single 4MB block in the cache.
// This is the fundamental storage unit - writes go directly into block buffers.
type blockBuffer struct {
	// data holds the block content (up to 4MB).
	// nil if the block has been evicted or not yet allocated.
	data []byte

	// coverage tracks which bytes have been written using a bitmap.
	// 1 bit per 64 bytes = 8KB for a 4MB block.
	// nil if no data has been written.
	coverage []uint64

	// state indicates whether this block is pending, uploading, or uploaded.
	state BlockState

	// dataSize tracks the actual bytes written (for partial blocks).
	// This is the highest (offset + length) seen, used for file size calculation.
	dataSize uint32

	// hash is the SHA-256 content hash computed when block becomes ReadyForUpload.
	// Used for deduplication lookup and as the block store key.
	// Zero value indicates hash not yet computed.
	hash [32]byte

	// uploadCancel cancels a pending upload when new writes arrive.
	// Set when transitioning to ReadyForUpload, called if write arrives before upload starts.
	uploadCancel func()
}

// PendingBlock represents a block ready for upload.
// Used by GetDirtyBlocks to return blocks that need flushing.
type PendingBlock struct {
	// ChunkIndex is the chunk this block belongs to.
	ChunkIndex uint32

	// BlockIndex is the index of this block within the chunk.
	BlockIndex uint32

	// Data is the block content. References cache's internal buffer - do not modify.
	Data []byte

	// Coverage is the coverage bitmap indicating which bytes are valid.
	Coverage []uint64

	// DataSize is the actual size of valid data in the block.
	DataSize uint32

	// Hash is the SHA-256 content hash (set when state is ReadyForUpload).
	// Zero value indicates hash not yet computed.
	Hash [32]byte

	// State is the current block state.
	State BlockState
}

// ============================================================================
// Cache Statistics
// ============================================================================

// Stats contains cache statistics for observability.
type Stats struct {
	// TotalSize is the current total size of cached data in bytes.
	TotalSize uint64

	// MaxSize is the configured maximum cache size (0 = unlimited).
	MaxSize uint64

	// FileCount is the number of files with cached data.
	FileCount int

	// DirtyBytes is the size of pending (unflushed) data.
	DirtyBytes uint64

	// UploadedBytes is the size of uploaded (evictable) data.
	UploadedBytes uint64

	// BlockCount is the total number of block buffers across all files.
	BlockCount int
}

// ============================================================================
// Coverage Bitmap Helpers
// ============================================================================

// newCoverageBitmap creates a new coverage bitmap for a block.
func newCoverageBitmap() []uint64 {
	return make([]uint64, CoverageWordsPerBlock)
}

// markCoverage sets bits in the coverage bitmap for a byte range.
// offset and length are relative to the block start.
func markCoverage(coverage []uint64, offset, length uint32) {
	if length == 0 || coverage == nil {
		return
	}

	// Convert to bit positions (1 bit per 64 bytes)
	startBit := offset / CoverageGranularity
	endBit := (offset + length - 1) / CoverageGranularity

	for bit := startBit; bit <= endBit; bit++ {
		wordIdx := bit / CoverageBitsPerWord
		bitInWord := bit % CoverageBitsPerWord
		if wordIdx < uint32(len(coverage)) {
			coverage[wordIdx] |= 1 << bitInWord
		}
	}
}

// isRangeCovered checks if all bytes in a range are covered.
// offset and length are relative to the block start.
func isRangeCovered(coverage []uint64, offset, length uint32) bool {
	if length == 0 {
		return true
	}
	if coverage == nil {
		return false
	}

	// Convert to bit positions (1 bit per 64 bytes)
	startBit := offset / CoverageGranularity
	endBit := (offset + length - 1) / CoverageGranularity

	for bit := startBit; bit <= endBit; bit++ {
		wordIdx := bit / CoverageBitsPerWord
		bitInWord := bit % CoverageBitsPerWord
		if wordIdx >= uint32(len(coverage)) {
			return false
		}
		if coverage[wordIdx]&(1<<bitInWord) == 0 {
			return false
		}
	}

	return true
}

// isFullyCovered checks if all bytes in a block are covered.
func isFullyCovered(coverage []uint64) bool {
	if coverage == nil {
		return false
	}
	for _, word := range coverage {
		if word != ^uint64(0) {
			return false
		}
	}
	return true
}
