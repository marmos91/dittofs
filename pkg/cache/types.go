// Package cache implements buffering for content stores.
package cache

import (
	"errors"

	"github.com/marmos91/dittofs/pkg/cache/wal"
	"github.com/marmos91/dittofs/pkg/payload/block"
	"github.com/marmos91/dittofs/pkg/payload/chunk"
)

// Re-export chunk constants for backward compatibility.
// New code should import pkg/payload/chunk and pkg/payload/block directly.
const (
	ChunkSize                = chunk.Size
	DefaultBlockSize         = block.Size
	MinBlockSize             = block.MinSize
	MaxBlockSize             = block.MaxSize
	DefaultMaxSlicesPerChunk = chunk.DefaultMaxSlicesPerChunk
)

// ============================================================================
// Errors
// ============================================================================

var (
	// ErrCacheClosed is returned when operations are attempted on a closed cache.
	ErrCacheClosed = errors.New("cache is closed")

	// ErrSliceNotFound is returned when a requested slice doesn't exist.
	ErrSliceNotFound = errors.New("slice not found")

	// ErrFileNotInCache is returned when a file has no cached data.
	ErrFileNotInCache = errors.New("file not in cache")

	// ErrInvalidChunkIndex is returned for out-of-range chunk indices.
	ErrInvalidChunkIndex = errors.New("invalid chunk index")

	// ErrInvalidOffset is returned for invalid slice offsets.
	ErrInvalidOffset = errors.New("invalid offset")

	// ErrCacheFull is returned when the cache is full of pending data that
	// cannot be evicted. This provides backpressure to prevent OOM conditions.
	// The caller should flush data (NFS COMMIT) before retrying the write.
	ErrCacheFull = errors.New("cache full: pending data cannot be evicted")
)

// ============================================================================
// Re-exported WAL Types
// ============================================================================

// SliceState represents the state of a slice in the cache.
// Defined in wal package for persistence format independence.
type SliceState = wal.SliceState

// SliceState constants - re-exported from wal package.
const (
	SliceStatePending   = wal.SliceStatePending
	SliceStateFlushed   = wal.SliceStateFlushed
	SliceStateUploading = wal.SliceStateUploading
)

// BlockRef references an immutable block in the block store.
// Defined in wal package for persistence format independence.
type BlockRef = wal.BlockRef

// ============================================================================
// Slice Types
// ============================================================================

// Slice represents a slice stored in the cache/store.
// Defined in wal package - re-exported here for convenience.
type Slice = wal.Slice

// PendingSlice represents an unflushed write ready for upload.
// Embeds Slice and adds ChunkIndex context for the transfer manager.
type PendingSlice struct {
	Slice

	// ChunkIndex is the chunk this slice belongs to.
	ChunkIndex uint32
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

	// FlushedBytes is the size of flushed (evictable) data.
	FlushedBytes uint64

	// SliceCount is the total number of slices across all files.
	SliceCount int
}
