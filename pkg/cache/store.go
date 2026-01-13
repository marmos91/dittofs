// Package cache implements buffering for content stores.
//
// This file defines the Store interface for cache persistence.
// Following the same pattern as pkg/metadata/store.go, the interface
// is at package root while implementations are in store/{memory,fs}/.
package cache

import (
	"context"
	"errors"
	"time"
)

// ============================================================================
// Store Errors
// ============================================================================

var (
	// ErrStoreClosed is returned when operations are attempted on a closed store.
	ErrStoreClosed = errors.New("store is closed")

	// ErrFileNotFound is returned when a file has no stored data.
	ErrFileNotFound = errors.New("file not found in store")

	// ErrStoreSliceNotFound is returned when a requested slice doesn't exist.
	ErrStoreSliceNotFound = errors.New("slice not found")

	// ErrChunkNotFound is returned when a requested chunk doesn't exist.
	ErrChunkNotFound = errors.New("chunk not found")
)

// ============================================================================
// Stored Types
// ============================================================================

// Slice represents a slice stored in the persistence layer.
//
// This is the storage representation - it contains all the data needed
// to persist and retrieve a slice, but no business logic.
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

// ============================================================================
// Store Interface
// ============================================================================

// Store defines the persistence operations for the cache layer.
//
// This interface handles low-level storage of slice data. The cache layer
// uses this interface to persist and retrieve slices, while keeping all
// business logic (merging, coalescing, sequential write optimization) separate.
//
// Thread Safety:
// All methods must be safe for concurrent use by multiple goroutines.
// Operations on different files should not block each other.
//
// Implementations:
//   - store/memory: In-memory storage (volatile, fast)
//   - store/fs: Filesystem-backed storage (persistent, future)
type Store interface {
	// ========================================================================
	// File Operations
	// ========================================================================

	// CreateFile ensures a file entry exists in the store.
	// If the file already exists, this is a no-op.
	// Returns error if store is closed.
	CreateFile(ctx context.Context, fileHandle []byte) error

	// FileExists returns true if the file has any stored data.
	FileExists(ctx context.Context, fileHandle []byte) bool

	// RemoveFile removes all stored data for a file.
	// Returns nil if file doesn't exist (idempotent).
	RemoveFile(ctx context.Context, fileHandle []byte) error

	// ListFiles returns all file handles with stored data.
	ListFiles(ctx context.Context) [][]byte

	// ========================================================================
	// Chunk Operations
	// ========================================================================

	// GetChunkIndices returns all chunk indices for a file.
	// Returns empty slice if file doesn't exist.
	GetChunkIndices(ctx context.Context, fileHandle []byte) []uint32

	// RemoveChunk removes a specific chunk and all its slices.
	// Returns nil if chunk doesn't exist (idempotent).
	RemoveChunk(ctx context.Context, fileHandle []byte, chunkIdx uint32) error

	// ========================================================================
	// Slice Operations
	// ========================================================================

	// GetSlices returns all slices for a chunk.
	// Slices are returned in the order they were stored (newest first).
	// Returns empty slice if chunk doesn't exist.
	GetSlices(ctx context.Context, fileHandle []byte, chunkIdx uint32) []Slice

	// AddSlice adds a new slice to a chunk.
	// The slice is prepended (newest first) to the existing slices.
	// Creates the file and chunk entries if they don't exist.
	AddSlice(ctx context.Context, fileHandle []byte, chunkIdx uint32, slice Slice) error

	// UpdateSlice updates an existing slice in a chunk.
	// Only the fields set in SliceUpdate are modified.
	// Returns ErrStoreSliceNotFound if slice doesn't exist.
	UpdateSlice(ctx context.Context, fileHandle []byte, chunkIdx uint32, sliceID string, update SliceUpdate) error

	// SetSlices replaces all slices for a chunk.
	// Used after coalescing to replace fragmented slices with merged ones.
	// Creates the file and chunk entries if they don't exist.
	SetSlices(ctx context.Context, fileHandle []byte, chunkIdx uint32, slices []Slice) error

	// FindSlice finds a slice by ID across all chunks.
	// Returns the chunk index and slice, or ErrStoreSliceNotFound if not found.
	FindSlice(ctx context.Context, fileHandle []byte, sliceID string) (chunkIdx uint32, slice *Slice, err error)

	// ExtendAdjacentSlice atomically attempts to extend a pending slice that is
	// adjacent to the new write.
	//
	// Parameters:
	//   - fileHandle, chunkIdx: Location of the slices
	//   - offset: The offset where new data starts
	//   - data: The data to append or prepend
	//
	// The method atomically:
	//   1. Finds a pending slice that ends at offset (append) or starts at offset+len(data) (prepend)
	//   2. Extends the slice with the new data
	//   3. Returns true if a slice was extended, false if no matching slice was found
	//
	// This avoids the TOCTOU race condition in the GetSlices+UpdateSlice pattern.
	ExtendAdjacentSlice(ctx context.Context, fileHandle []byte, chunkIdx uint32, offset uint32, data []byte) bool

	// CoalesceChunk atomically merges adjacent pending slices within a chunk.
	//
	// This method holds the lock during the entire read-merge-write operation to
	// avoid TOCTOU race conditions with concurrent writes.
	//
	// The coalescing algorithm:
	//   1. Separates pending slices from other slices
	//   2. Sorts pending slices by offset
	//   3. Merges adjacent/overlapping pending slices
	//   4. Replaces the chunk's slices with merged pending + unchanged other slices
	//
	// Returns nil if file or chunk doesn't exist (no-op).
	CoalesceChunk(ctx context.Context, fileHandle []byte, chunkIdx uint32) error

	// ========================================================================
	// Size Tracking
	// ========================================================================

	// GetTotalSize returns the total bytes stored in the cache.
	GetTotalSize() uint64

	// AddSize adds to the total size counter.
	// Use negative values to subtract.
	AddSize(delta int64)

	// ========================================================================
	// Lifecycle
	// ========================================================================

	// IsClosed returns true if the store has been closed.
	IsClosed() bool

	// Close releases all store resources.
	// After Close, all operations return ErrStoreClosed.
	Close() error
}
