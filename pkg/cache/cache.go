// Package cache implements buffering for content stores.
//
// Cache provides a slice-aware caching layer for the Chunk/Slice/Block
// storage model. It buffers writes as slices and serves reads by merging
// slices (newest-wins semantics).
//
// Key Design Principles:
//   - Slice-aware: WriteSlice/ReadSlice API maps directly to data model
//   - Storage-backend agnostic: Cache doesn't know about S3/filesystem/etc.
//   - Mandatory: All content operations go through the cache
//   - Write coalescing: Adjacent writes merged before flush
//   - Newest-wins reads: Overlapping slices resolved by creation time
//
// Architecture:
//
//	Cache (interface, business logic)
//	    ↓
//	store.Store (interface, persistence)
//	    ↓
//	store/memory/ or store/fs/ (implementations)
//
// See docs/ARCHITECTURE.md for the full Chunk/Slice/Block model.
package cache

import (
	"context"
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

// CachedSlice represents a slice stored in the cache.
//
// Slices are ordered by CreatedAt (newest first) within a chunk.
// On reads, newer slices take precedence for overlapping ranges.
type CachedSlice struct {
	// ID uniquely identifies this slice (used for flush tracking).
	ID string

	// ChunkIndex is the chunk this slice belongs to (offset / ChunkSize).
	ChunkIndex uint32

	// Offset is the byte offset within the chunk (0 to ChunkSize-1).
	Offset uint32

	// Length is the size of this slice in bytes.
	Length uint32

	// Data contains the actual slice content.
	// For flushed slices, this may be nil (data is in block storage).
	Data []byte

	// State indicates whether this slice is pending, uploading, or flushed.
	State SliceState

	// CreatedAt is when this slice was created (for newest-wins ordering).
	CreatedAt time.Time

	// BlockRefs contains references to blocks after flushing.
	// Empty for pending slices.
	BlockRefs []BlockRef
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
// Cache Interface
// ============================================================================

// Cache is the mandatory cache layer for all content operations.
//
// It understands slices as first-class citizens but is agnostic to the
// underlying storage backend (S3, filesystem, memory, Redis).
//
// The Cache handles business logic (slice merging, coalescing,
// sequential write optimization) while delegating persistence to a
// store.Store implementation.
//
// Thread Safety:
// All methods must be safe for concurrent use by multiple goroutines.
// Operations on different files should not block each other.
type Cache interface {
	// ========================================================================
	// Write Operations
	// ========================================================================

	// WriteSlice writes a slice to the cache (pending until flushed).
	//
	// This creates a new slice at the specified position within the chunk.
	// The slice is marked as pending and will be included in GetDirtySlices.
	//
	// Optimization: Sequential writes are automatically coalesced by extending
	// existing pending slices rather than creating new ones.
	//
	// Parameters:
	//   - ctx: Context for cancellation
	//   - fileHandle: Identifies the file (opaque bytes)
	//   - chunkIdx: Which chunk (0, 1, 2, ...) based on file offset / ChunkSize
	//   - data: The bytes to write
	//   - offset: Offset within the chunk (0 to ChunkSize-1)
	//
	// The offset + len(data) must not exceed ChunkSize.
	// Returns error if cache is closed or parameters are invalid.
	WriteSlice(ctx context.Context, fileHandle []byte, chunkIdx uint32, data []byte, offset uint32) error

	// ========================================================================
	// Read Operations
	// ========================================================================

	// ReadSlice reads data from cache, merging pending slices with flushed ones.
	//
	// Reads data from the cache for the specified chunk and byte range.
	// Returns ErrFileNotInCache if the file has no cached data.
	//
	// The returned data may be:
	//   - Fully from pending slices (not yet flushed)
	//   - Fully from flushed slices (still in cache)
	//   - A merge of both (newest wins for overlaps)
	//   - Zeros for unwritten regions
	//
	// Parameters:
	//   - ctx: Context for cancellation
	//   - fileHandle: Identifies the file
	//   - chunkIdx: Which chunk to read from
	//   - offset: Offset within the chunk
	//   - length: Number of bytes to read
	//
	// Returns:
	//   - []byte: The requested data (may include zeros for sparse regions)
	//   - bool: True if data was found in cache, false if cache miss
	//   - error: ErrFileNotInCache, ErrCacheClosed, or context error
	ReadSlice(ctx context.Context, fileHandle []byte, chunkIdx uint32, offset, length uint32) ([]byte, bool, error)

	// ========================================================================
	// Flush Coordination
	// ========================================================================

	// GetDirtySlices returns all pending (unflushed) slices for a file.
	//
	// This is called before flushing to get all data that needs to be uploaded.
	// The returned slices have been coalesced (adjacent writes merged).
	//
	// Parameters:
	//   - ctx: Context for cancellation
	//   - fileHandle: Identifies the file
	//
	// Returns:
	//   - []PendingSlice: Slices ordered by chunk index, then offset
	//   - error: ErrFileNotInCache if no cached data, ErrCacheClosed if closed
	GetDirtySlices(ctx context.Context, fileHandle []byte) ([]PendingSlice, error)

	// MarkSliceFlushed marks a slice as successfully flushed to block storage.
	//
	// After a slice is uploaded to the block store, call this to:
	//   - Transition the slice from pending to flushed state
	//   - Store the block references for future reads
	//   - Allow the slice to be evicted from cache
	//
	// Parameters:
	//   - ctx: Context for cancellation
	//   - fileHandle: Identifies the file
	//   - sliceID: The ID of the slice that was flushed
	//   - blockRefs: References to the blocks where data was stored
	//
	// Returns error if slice not found or cache is closed.
	MarkSliceFlushed(ctx context.Context, fileHandle []byte, sliceID string, blockRefs []BlockRef) error

	// ========================================================================
	// Write Optimization
	// ========================================================================

	// CoalesceWrites merges adjacent pending writes into fewer slices.
	//
	// Called before flush to optimize by combining adjacent writes.
	// For example, writes at offsets [0, 1024, 2048] with length 1024 each
	// become a single slice at offset 0 with length 3072.
	//
	// This is called automatically by GetDirtySlices, but can be called
	// explicitly for optimization.
	//
	// Parameters:
	//   - ctx: Context for cancellation
	//   - fileHandle: Identifies the file
	//
	// Returns error if file not in cache or cache is closed.
	CoalesceWrites(ctx context.Context, fileHandle []byte) error

	// ========================================================================
	// Cache Management
	// ========================================================================

	// Evict removes cached data for a file (only flushed slices).
	//
	// This removes all flushed slices from the cache. Pending (dirty) slices
	// are protected and cannot be evicted.
	//
	// Parameters:
	//   - ctx: Context for cancellation
	//   - fileHandle: Identifies the file
	//
	// Returns:
	//   - uint64: Number of bytes evicted
	//   - error: ErrCacheClosed if closed
	Evict(ctx context.Context, fileHandle []byte) (uint64, error)

	// EvictAll removes all flushed data from the cache.
	//
	// Used for cache pressure relief. Dirty slices are protected.
	//
	// Returns:
	//   - uint64: Number of bytes evicted
	//   - error: ErrCacheClosed if closed
	EvictAll(ctx context.Context) (uint64, error)

	// Remove completely removes all cached data for a file.
	//
	// Unlike Evict, this removes both pending and flushed slices.
	// Use this when a file is deleted.
	//
	// Parameters:
	//   - ctx: Context for cancellation
	//   - fileHandle: Identifies the file
	//
	// Returns error if cache is closed.
	Remove(ctx context.Context, fileHandle []byte) error

	// Truncate changes the size of cached data for a file.
	//
	// If newSize < currentSize: Data beyond newSize is removed.
	// If newSize > currentSize: No change (sparse file semantics).
	// If newSize == currentSize: No-op.
	//
	// Parameters:
	//   - ctx: Context for cancellation
	//   - fileHandle: Identifies the file
	//   - newSize: The new file size in bytes
	//
	// Returns error if cache is closed.
	Truncate(ctx context.Context, fileHandle []byte, newSize uint64) error

	// ========================================================================
	// File State
	// ========================================================================

	// HasDirtyData returns true if the file has any pending (unflushed) slices.
	//
	// Parameters:
	//   - fileHandle: Identifies the file
	//
	// Returns false if file is not in cache or has no dirty data.
	HasDirtyData(fileHandle []byte) bool

	// GetFileSize returns the size of cached data for a file.
	//
	// This returns the maximum extent of all cached slices.
	// May not represent the actual file size (sparse files, partial cache).
	//
	// Parameters:
	//   - fileHandle: Identifies the file
	//
	// Returns 0 if file is not in cache.
	GetFileSize(fileHandle []byte) uint64

	// ListFiles returns all file handles with cached data.
	//
	// Returns empty slice if no files are cached or cache is closed.
	ListFiles() [][]byte

	// ========================================================================
	// Lifecycle
	// ========================================================================

	// Close releases all cache resources.
	//
	// After Close:
	//   - All pending data is lost (not flushed!)
	//   - All operations return ErrCacheClosed
	//
	// Callers should flush dirty data before calling Close.
	Close() error
}

// ============================================================================
// Helper Functions
// ============================================================================

// ChunkIndexForOffset calculates the chunk index for a file offset.
func ChunkIndexForOffset(offset uint64) uint32 {
	return uint32(offset / ChunkSize)
}

// OffsetWithinChunk calculates the offset within a chunk.
func OffsetWithinChunk(offset uint64) uint32 {
	return uint32(offset % ChunkSize)
}

// ChunkRange calculates the range of chunks that a byte range spans.
// Returns startChunk and endChunk (inclusive).
func ChunkRange(offset, length uint64) (startChunk, endChunk uint32) {
	if length == 0 {
		return ChunkIndexForOffset(offset), ChunkIndexForOffset(offset)
	}
	startChunk = ChunkIndexForOffset(offset)
	endChunk = ChunkIndexForOffset(offset + length - 1)
	return startChunk, endChunk
}
