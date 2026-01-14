// Package cache implements buffering for content stores.
package cache

import (
	"errors"
	"time"

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

// SliceUpdate contains fields that can be updated on an existing slice.
type SliceUpdate struct {
	// State is the new state (optional).
	State *SliceState

	// BlockRefs is the new block references (optional).
	BlockRefs []BlockRef

	// Data is new data for the slice (optional, used for extending).
	Data []byte

	// Length is the new length (optional, used for extending/truncating).
	Length *uint32

	// Offset is the new offset (optional, used for prepending).
	Offset *uint32
}

// PendingSlice represents an unflushed write ready for upload.
type PendingSlice struct {
	// ID uniquely identifies this slice.
	ID string

	// ChunkIndex is the chunk this slice belongs to.
	ChunkIndex uint32

	// Offset within the chunk.
	Offset uint32

	// Length of the slice data.
	Length uint32

	// Data contains the actual bytes to upload.
	Data []byte

	// CreatedAt for ordering.
	CreatedAt time.Time
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
