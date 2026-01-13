// Package cache implements buffering for content stores.
//
// This file defines the Store interface for cache persistence.
// Following the same pattern as pkg/metadata/store.go, the interface
// is at package root while implementations are in store/{memory,fs}/.
package cache

import (
	"context"
	"errors"
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
// Store Interface
// ============================================================================

// Store defines the persistence operations for the cache layer.
//
// This interface handles low-level storage of slice data. It is a simple
// persistence layer that only provides CRUD operations for slices.
// All business logic (merging, coalescing, sequential write optimization)
// belongs in the Cache layer.
//
// Thread Safety:
// Implementations must provide basic thread-safety for map operations.
// The Cache layer provides additional per-file locking for finer-grained
// concurrency control.
//
// Implementations:
//   - store/memory: In-memory storage (volatile, fast)
//   - store/filesystem: Filesystem-backed storage (persistent, future)
type Store interface {
	// ========================================================================
	// File Operations
	// ========================================================================

	// CreateFile ensures a file entry exists in the store.
	CreateFile(ctx context.Context, fileHandle []byte) error

	// FileExists returns true if the file has any stored data.
	FileExists(ctx context.Context, fileHandle []byte) bool

	// RemoveFile removes all stored data for a file.
	RemoveFile(ctx context.Context, fileHandle []byte) error

	// ListFiles returns all file handles with stored data.
	ListFiles(ctx context.Context) [][]byte

	// ========================================================================
	// Chunk Operations
	// ========================================================================

	// GetChunkIndices returns all chunk indices for a file.
	GetChunkIndices(ctx context.Context, fileHandle []byte) []uint32

	// RemoveChunk removes a specific chunk and all its slices.
	RemoveChunk(ctx context.Context, fileHandle []byte, chunkIdx uint32) error

	// ========================================================================
	// Slice Operations
	// ========================================================================

	// GetSlices returns all slices for a chunk (newest first).
	GetSlices(ctx context.Context, fileHandle []byte, chunkIdx uint32) []Slice

	// AddSlice adds a new slice to a chunk (prepended, newest first).
	AddSlice(ctx context.Context, fileHandle []byte, chunkIdx uint32, slice Slice) error

	// UpdateSlice updates an existing slice in a chunk.
	UpdateSlice(ctx context.Context, fileHandle []byte, chunkIdx uint32, sliceID string, update SliceUpdate) error

	// SetSlices replaces all slices for a chunk.
	SetSlices(ctx context.Context, fileHandle []byte, chunkIdx uint32, slices []Slice) error

	// FindSlice finds a slice by ID across all chunks.
	FindSlice(ctx context.Context, fileHandle []byte, sliceID string) (chunkIdx uint32, slice *Slice, err error)

	// ExtendSliceData efficiently extends a slice's data in place.
	// Uses Go's append() for amortized O(1) growth.
	ExtendSliceData(ctx context.Context, fileHandle []byte, chunkIdx uint32, sliceID string, data []byte, appendMode bool) error

	// TryExtendAdjacentSlice atomically finds and extends an adjacent pending slice.
	// Returns true if extended, false if no adjacent slice found.
	TryExtendAdjacentSlice(ctx context.Context, fileHandle []byte, chunkIdx uint32, offset uint32, data []byte) bool

	// ========================================================================
	// Size Tracking
	// ========================================================================

	// GetTotalSize returns the total bytes stored.
	GetTotalSize() uint64

	// AddSize adds to the total size counter (negative to subtract).
	AddSize(delta int64)

	// ========================================================================
	// Lifecycle
	// ========================================================================

	// IsClosed returns true if the store has been closed.
	IsClosed() bool

	// Close releases all store resources.
	Close() error
}
