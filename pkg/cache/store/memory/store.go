// Package memory provides an in-memory implementation of the cache store.
//
// This implementation stores all slice data in memory. It is volatile and
// will lose all data when the process exits. Use it for:
//   - Testing
//   - Development
//   - Cache-only configurations (Phase 1)
package memory

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/cache"
)

// Compile-time check that Store implements cache.Store.
var _ cache.Store = (*Store)(nil)

// ============================================================================
// Internal Types
// ============================================================================

// fileEntry holds all stored data for a single file.
type fileEntry struct {
	mu     sync.RWMutex
	chunks map[uint32]*chunkEntry // chunkIndex -> chunkEntry
}

// chunkEntry holds all slices for a single chunk.
type chunkEntry struct {
	slices []cache.Slice // Ordered newest-first (prepended on add)
}

// ============================================================================
// Store
// ============================================================================

// Store implements store.Store with in-memory storage.
//
// Thread Safety:
// Two-level locking for efficiency:
//   - s.mu (RWMutex): Protects the files map structure
//   - file.mu (RWMutex): Protects individual file's chunks and slices
//
// This allows concurrent operations on different files without contention.
type Store struct {
	mu        sync.RWMutex
	files     map[string]*fileEntry // fileHandle (as string) -> fileEntry
	closed    bool
	totalSize atomic.Uint64
}

// New creates a new in-memory store.
func New() *Store {
	return &Store{
		files: make(map[string]*fileEntry),
	}
}

// ============================================================================
// File Operations
// ============================================================================

// CreateFile ensures a file entry exists in the store.
func (s *Store) CreateFile(ctx context.Context, fileHandle []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	key := string(fileHandle)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return cache.ErrStoreClosed
	}

	if _, exists := s.files[key]; !exists {
		s.files[key] = &fileEntry{
			chunks: make(map[uint32]*chunkEntry),
		}
	}

	return nil
}

// FileExists returns true if the file has any stored data.
func (s *Store) FileExists(ctx context.Context, fileHandle []byte) bool {
	key := string(fileHandle)

	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return false
	}

	_, exists := s.files[key]
	return exists
}

// RemoveFile removes all stored data for a file.
func (s *Store) RemoveFile(ctx context.Context, fileHandle []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	key := string(fileHandle)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return cache.ErrStoreClosed
	}

	file, exists := s.files[key]
	if !exists {
		return nil // Idempotent
	}

	// Calculate size to subtract
	file.mu.Lock()
	var size uint64
	for _, chunk := range file.chunks {
		for _, slice := range chunk.slices {
			size += uint64(len(slice.Data))
		}
	}
	file.mu.Unlock()

	delete(s.files, key)

	if size > 0 {
		s.totalSize.Add(^(size - 1)) // Subtract
	}

	return nil
}

// ListFiles returns all file handles with stored data.
func (s *Store) ListFiles(ctx context.Context) [][]byte {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return [][]byte{}
	}

	result := make([][]byte, 0, len(s.files))
	for key := range s.files {
		result = append(result, []byte(key))
	}

	return result
}

// ============================================================================
// Chunk Operations
// ============================================================================

// GetChunkIndices returns all chunk indices for a file.
func (s *Store) GetChunkIndices(ctx context.Context, fileHandle []byte) []uint32 {
	file := s.getFile(fileHandle)
	if file == nil {
		return []uint32{}
	}

	file.mu.RLock()
	defer file.mu.RUnlock()

	result := make([]uint32, 0, len(file.chunks))
	for idx := range file.chunks {
		result = append(result, idx)
	}

	return result
}

// RemoveChunk removes a specific chunk and all its slices.
func (s *Store) RemoveChunk(ctx context.Context, fileHandle []byte, chunkIdx uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	file := s.getFile(fileHandle)
	if file == nil {
		return nil // Idempotent
	}

	file.mu.Lock()
	defer file.mu.Unlock()

	chunk, exists := file.chunks[chunkIdx]
	if !exists {
		return nil // Idempotent
	}

	// Calculate size to subtract
	var size uint64
	for _, slice := range chunk.slices {
		size += uint64(len(slice.Data))
	}

	delete(file.chunks, chunkIdx)

	if size > 0 {
		s.totalSize.Add(^(size - 1)) // Subtract
	}

	return nil
}

// ============================================================================
// Slice Operations
// ============================================================================

// GetSlices returns all slices for a chunk.
func (s *Store) GetSlices(ctx context.Context, fileHandle []byte, chunkIdx uint32) []cache.Slice {
	file := s.getFile(fileHandle)
	if file == nil {
		return []cache.Slice{}
	}

	file.mu.RLock()
	defer file.mu.RUnlock()

	chunk, exists := file.chunks[chunkIdx]
	if !exists {
		return []cache.Slice{}
	}

	// Return a copy to prevent external modification
	result := make([]cache.Slice, len(chunk.slices))
	for i, slice := range chunk.slices {
		result[i] = copySlice(slice)
	}

	return result
}

// AddSlice adds a new slice to a chunk.
func (s *Store) AddSlice(ctx context.Context, fileHandle []byte, chunkIdx uint32, slice cache.Slice) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	file, err := s.getOrCreateFile(fileHandle)
	if err != nil {
		return err
	}

	file.mu.Lock()
	defer file.mu.Unlock()

	chunk, exists := file.chunks[chunkIdx]
	if !exists {
		chunk = &chunkEntry{
			slices: make([]cache.Slice, 0),
		}
		file.chunks[chunkIdx] = chunk
	}

	// Make a copy of the data
	sliceCopy := copySlice(slice)

	// Prepend to slices (newest first)
	chunk.slices = append([]cache.Slice{sliceCopy}, chunk.slices...)

	// Update total size
	s.totalSize.Add(uint64(len(slice.Data)))

	return nil
}

// UpdateSlice updates an existing slice in a chunk.
func (s *Store) UpdateSlice(ctx context.Context, fileHandle []byte, chunkIdx uint32, sliceID string, update cache.SliceUpdate) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	file := s.getFile(fileHandle)
	if file == nil {
		return cache.ErrFileNotFound
	}

	file.mu.Lock()
	defer file.mu.Unlock()

	chunk, exists := file.chunks[chunkIdx]
	if !exists {
		return cache.ErrChunkNotFound
	}

	for i := range chunk.slices {
		if chunk.slices[i].ID == sliceID {
			oldSize := uint64(len(chunk.slices[i].Data))

			// Apply updates
			if update.State != nil {
				chunk.slices[i].State = *update.State
			}
			if update.BlockRefs != nil {
				chunk.slices[i].BlockRefs = make([]cache.BlockRef, len(update.BlockRefs))
				copy(chunk.slices[i].BlockRefs, update.BlockRefs)
			}
			if update.Data != nil {
				chunk.slices[i].Data = make([]byte, len(update.Data))
				copy(chunk.slices[i].Data, update.Data)
			}
			if update.Length != nil {
				chunk.slices[i].Length = *update.Length
			}
			if update.Offset != nil {
				chunk.slices[i].Offset = *update.Offset
			}

			// Update size tracking if data changed
			if update.Data != nil {
				newSize := uint64(len(update.Data))
				if newSize > oldSize {
					s.totalSize.Add(newSize - oldSize)
				} else if oldSize > newSize {
					s.totalSize.Add(^(oldSize - newSize - 1)) // Subtract
				}
			}

			return nil
		}
	}

	return cache.ErrStoreSliceNotFound
}

// SetSlices replaces all slices for a chunk.
func (s *Store) SetSlices(ctx context.Context, fileHandle []byte, chunkIdx uint32, slices []cache.Slice) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	file, err := s.getOrCreateFile(fileHandle)
	if err != nil {
		return err
	}

	file.mu.Lock()
	defer file.mu.Unlock()

	// Calculate old size
	var oldSize uint64
	if chunk, exists := file.chunks[chunkIdx]; exists {
		for _, slice := range chunk.slices {
			oldSize += uint64(len(slice.Data))
		}
	}

	// Calculate new size and copy slices
	var newSize uint64
	newSlices := make([]cache.Slice, len(slices))
	for i, slice := range slices {
		newSlices[i] = copySlice(slice)
		newSize += uint64(len(slice.Data))
	}

	// Update chunk
	file.chunks[chunkIdx] = &chunkEntry{slices: newSlices}

	// Update size tracking
	if newSize > oldSize {
		s.totalSize.Add(newSize - oldSize)
	} else if oldSize > newSize {
		s.totalSize.Add(^(oldSize - newSize - 1)) // Subtract
	}

	return nil
}

// FindSlice finds a slice by ID across all chunks.
func (s *Store) FindSlice(ctx context.Context, fileHandle []byte, sliceID string) (uint32, *cache.Slice, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}

	file := s.getFile(fileHandle)
	if file == nil {
		return 0, nil, cache.ErrFileNotFound
	}

	file.mu.RLock()
	defer file.mu.RUnlock()

	for chunkIdx, chunk := range file.chunks {
		for _, slice := range chunk.slices {
			if slice.ID == sliceID {
				sliceCopy := copySlice(slice)
				return chunkIdx, &sliceCopy, nil
			}
		}
	}

	return 0, nil, cache.ErrStoreSliceNotFound
}

// ExtendAdjacentSlice atomically attempts to extend a pending slice that is adjacent
// to the new write. This avoids the TOCTOU race condition by holding the lock during
// the entire read-check-extend operation.
//
// Returns true if an adjacent slice was found and extended, false otherwise.
// When false is returned, the caller should create a new slice.
func (s *Store) ExtendAdjacentSlice(ctx context.Context, fileHandle []byte, chunkIdx uint32, offset uint32, data []byte) bool {
	if ctx.Err() != nil {
		return false
	}

	file := s.getFile(fileHandle)
	if file == nil {
		return false
	}

	file.mu.Lock()
	defer file.mu.Unlock()

	chunk, exists := file.chunks[chunkIdx]
	if !exists {
		return false
	}

	writeEnd := offset + uint32(len(data))

	for i := range chunk.slices {
		slice := &chunk.slices[i]
		if slice.State != cache.SliceStatePending {
			continue
		}

		sliceEnd := slice.Offset + slice.Length

		// Case 1: Appending (write starts where slice ends)
		if offset == sliceEnd {
			// Extend using Go's append for amortized O(1) growth
			oldLen := len(slice.Data)
			slice.Data = append(slice.Data, data...)
			slice.Length = slice.Length + uint32(len(data))

			// Update size tracking
			s.totalSize.Add(uint64(len(slice.Data) - oldLen))
			return true
		}

		// Case 2: Prepending (write ends where slice starts)
		if writeEnd == slice.Offset {
			// Prepend the data in place
			oldSize := uint64(len(slice.Data))
			newData := make([]byte, len(data)+len(slice.Data))
			copy(newData, data)
			copy(newData[len(data):], slice.Data)

			slice.Data = newData
			slice.Offset = offset
			slice.Length = slice.Length + uint32(len(data))

			// Update size tracking
			newSize := uint64(len(newData))
			if newSize > oldSize {
				s.totalSize.Add(newSize - oldSize)
			}
			return true
		}
	}

	return false
}

// CoalesceChunk atomically merges adjacent pending slices within a chunk.
// This holds the lock during the entire operation to avoid TOCTOU races.
func (s *Store) CoalesceChunk(ctx context.Context, fileHandle []byte, chunkIdx uint32) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	file := s.getFile(fileHandle)
	if file == nil {
		return nil // No-op for non-existent file
	}

	file.mu.Lock()
	defer file.mu.Unlock()

	chunk, exists := file.chunks[chunkIdx]
	if !exists || len(chunk.slices) <= 1 {
		return nil // Nothing to coalesce
	}

	// Separate pending and non-pending slices
	var pending []cache.Slice
	var other []cache.Slice

	for _, slice := range chunk.slices {
		if slice.State == cache.SliceStatePending {
			pending = append(pending, slice)
		} else {
			other = append(other, slice)
		}
	}

	if len(pending) <= 1 {
		return nil // Nothing to coalesce
	}

	// Sort pending slices by offset for coalescing
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].Offset < pending[j].Offset
	})

	// Merge adjacent or overlapping pending slices
	merged := make([]cache.Slice, 0)
	var current *cache.Slice

	for _, slice := range pending {
		if current == nil {
			// Start new merged slice
			newSlice := cache.Slice{
				ID:        uuid.New().String(),
				Offset:    slice.Offset,
				Length:    slice.Length,
				Data:      make([]byte, slice.Length),
				State:     cache.SliceStatePending,
				CreatedAt: time.Now(),
			}
			copy(newSlice.Data, slice.Data)
			current = &newSlice
			continue
		}

		currentEnd := current.Offset + current.Length

		if slice.Offset <= currentEnd {
			// Adjacent or overlapping - merge
			sliceEnd := slice.Offset + slice.Length
			newEnd := max(currentEnd, sliceEnd)
			newLength := newEnd - current.Offset

			// Extend data buffer if needed
			if newLength > uint32(len(current.Data)) {
				newData := make([]byte, newLength)
				copy(newData, current.Data)
				current.Data = newData
				current.Length = newLength
			}

			// Copy slice data (handles overlaps correctly since we process in offset order)
			dstOffset := slice.Offset - current.Offset
			copy(current.Data[dstOffset:], slice.Data)
		} else {
			// Gap - save current and start new
			merged = append(merged, *current)
			newSlice := cache.Slice{
				ID:        uuid.New().String(),
				Offset:    slice.Offset,
				Length:    slice.Length,
				Data:      make([]byte, slice.Length),
				State:     cache.SliceStatePending,
				CreatedAt: time.Now(),
			}
			copy(newSlice.Data, slice.Data)
			current = &newSlice
		}
	}

	// Don't forget the last one
	if current != nil {
		merged = append(merged, *current)
	}

	// Calculate old total size
	var oldSize uint64
	for _, slice := range chunk.slices {
		oldSize += uint64(len(slice.Data))
	}

	// Rebuild slices list: merged pending first (newest), then other slices
	newSlices := append(merged, other...)

	// Calculate new total size
	var newSize uint64
	for _, slice := range newSlices {
		newSize += uint64(len(slice.Data))
	}

	// Update chunk
	chunk.slices = newSlices

	// Update size tracking
	if newSize > oldSize {
		s.totalSize.Add(newSize - oldSize)
	} else if oldSize > newSize {
		s.totalSize.Add(^(oldSize - newSize - 1)) // Subtract
	}

	return nil
}

// ============================================================================
// Size Tracking
// ============================================================================

// GetTotalSize returns the total bytes stored in the cache.
func (s *Store) GetTotalSize() uint64 {
	return s.totalSize.Load()
}

// AddSize adds to the total size counter.
func (s *Store) AddSize(delta int64) {
	if delta >= 0 {
		s.totalSize.Add(uint64(delta))
	} else {
		s.totalSize.Add(^uint64(-delta - 1)) // Subtract
	}
}

// ============================================================================
// Lifecycle
// ============================================================================

// IsClosed returns true if the store has been closed.
func (s *Store) IsClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

// Close releases all store resources.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}

	s.files = nil
	s.totalSize.Store(0)
	s.closed = true

	return nil
}

// ============================================================================
// Helper Methods
// ============================================================================

// getFile retrieves an existing file entry.
func (s *Store) getFile(fileHandle []byte) *fileEntry {
	key := string(fileHandle)

	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil
	}

	return s.files[key]
}

// getOrCreateFile retrieves or creates a file entry.
func (s *Store) getOrCreateFile(fileHandle []byte) (*fileEntry, error) {
	key := string(fileHandle)

	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, cache.ErrStoreClosed
	}
	file, exists := s.files[key]
	s.mu.RUnlock()

	if exists {
		return file, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, cache.ErrStoreClosed
	}

	// Double-check after acquiring write lock
	file, exists = s.files[key]
	if !exists {
		file = &fileEntry{
			chunks: make(map[uint32]*chunkEntry),
		}
		s.files[key] = file
	}

	return file, nil
}

// copySlice creates a deep copy of a slice.
func copySlice(src cache.Slice) cache.Slice {
	dst := cache.Slice{
		ID:        src.ID,
		Offset:    src.Offset,
		Length:    src.Length,
		State:     src.State,
		CreatedAt: src.CreatedAt,
	}

	if src.Data != nil {
		dst.Data = make([]byte, len(src.Data))
		copy(dst.Data, src.Data)
	}

	if src.BlockRefs != nil {
		dst.BlockRefs = make([]cache.BlockRef, len(src.BlockRefs))
		copy(dst.BlockRefs, src.BlockRefs)
	}

	return dst
}
