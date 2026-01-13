// Package cache implements buffering for content stores.
package cache

import (
	"errors"
	"time"
)

// ============================================================================
// Constants
// ============================================================================

const (
	// ChunkSize is the size of a chunk in bytes (64MB).
	// Files are divided into chunks for metadata organization.
	ChunkSize = 64 * 1024 * 1024

	// DefaultBlockSize is the default block size for storage (4MB).
	// Each block becomes a single object in the block store.
	DefaultBlockSize = 4 * 1024 * 1024

	// MinBlockSize is the minimum allowed block size (1MB).
	MinBlockSize = 1 * 1024 * 1024

	// MaxBlockSize is the maximum allowed block size (16MB).
	MaxBlockSize = 16 * 1024 * 1024

	// DefaultMaxSlicesPerChunk is when compaction is triggered.
	DefaultMaxSlicesPerChunk = 16
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
// Slice State
// ============================================================================

// SliceState represents the state of a slice in the cache.
type SliceState int

const (
	// SliceStatePending indicates the slice has unflushed data.
	SliceStatePending SliceState = iota

	// SliceStateFlushed indicates the slice has been persisted to block storage.
	SliceStateFlushed

	// SliceStateUploading indicates the slice is currently being uploaded.
	SliceStateUploading
)

// String returns the string representation of SliceState.
func (s SliceState) String() string {
	switch s {
	case SliceStatePending:
		return "Pending"
	case SliceStateFlushed:
		return "Flushed"
	case SliceStateUploading:
		return "Uploading"
	default:
		return "Unknown"
	}
}

// IsDirty returns true if the slice has unflushed data.
func (s SliceState) IsDirty() bool {
	return s == SliceStatePending || s == SliceStateUploading
}

// ============================================================================
// Block Reference
// ============================================================================

// BlockRef references an immutable block in the block store.
type BlockRef struct {
	// ID is the block's unique identifier in the block store.
	ID string

	// Size is the actual size of this block (may be < BlockSize for last block).
	Size uint32
}

// ============================================================================
// Slice Types
// ============================================================================

// Slice represents a slice stored in the cache/store.
type Slice struct {
	// ID uniquely identifies this slice.
	ID string

	// Offset is the byte offset within the chunk (0 to ChunkSize-1).
	Offset uint32

	// Length is the size of this slice in bytes.
	Length uint32

	// Data contains the actual slice content.
	Data []byte

	// State indicates whether this slice is pending, uploading, or flushed.
	State SliceState

	// CreatedAt is when this slice was created (for newest-wins ordering).
	CreatedAt time.Time

	// BlockRefs contains references to blocks after flushing.
	BlockRefs []BlockRef
}

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
