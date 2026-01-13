// Package memory provides an in-memory implementation of the cache store.
//
// This implementation stores all slice data in memory. It is volatile and
// will lose all data when the process exits. Use it for:
//   - Testing
//   - Development
//   - Cache-only configurations (Phase 1)
//
// Thread Safety:
// This store provides basic thread-safety for map operations. The Cache
// layer provides additional per-file locking for finer-grained concurrency.
package memory

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/marmos91/dittofs/pkg/cache"
)

// Compile-time check that Store implements cache.Store.
var _ cache.Store = (*Store)(nil)

// ============================================================================
// Internal Types
// ============================================================================

// fileEntry holds all stored data for a single file.
type fileEntry struct {
	chunks map[uint32]*chunkEntry // chunkIndex -> chunkEntry
}

// chunkEntry holds all slices for a single chunk.
type chunkEntry struct {
	slices []cache.Slice // Ordered newest-first (prepended on add)
}

// ============================================================================
// Store
// ============================================================================

// Store implements cache.Store with in-memory storage.
//
// Thread Safety:
// The Store uses a simple mutex to protect its files map. This allows
// concurrent access from multiple goroutines while keeping the implementation
// simple. The Cache layer provides additional per-file locking for finer-grained
// concurrency control on individual file operations.
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

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return cache.ErrStoreClosed
	}

	key := string(fileHandle)
	if _, exists := s.files[key]; !exists {
		s.files[key] = &fileEntry{
			chunks: make(map[uint32]*chunkEntry),
		}
	}

	return nil
}

// FileExists returns true if the file has any stored data.
func (s *Store) FileExists(ctx context.Context, fileHandle []byte) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return false
	}

	key := string(fileHandle)
	_, exists := s.files[key]
	return exists
}

// RemoveFile removes all stored data for a file.
func (s *Store) RemoveFile(ctx context.Context, fileHandle []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return cache.ErrStoreClosed
	}

	key := string(fileHandle)
	file, exists := s.files[key]
	if !exists {
		return nil // Idempotent
	}

	// Calculate size to subtract
	var size uint64
	for _, chunk := range file.chunks {
		for _, slice := range chunk.slices {
			size += uint64(len(slice.Data))
		}
	}

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
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := string(fileHandle)
	file, exists := s.files[key]
	if !exists {
		return []uint32{}
	}

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

	s.mu.Lock()
	defer s.mu.Unlock()

	key := string(fileHandle)
	file, exists := s.files[key]
	if !exists {
		return nil // Idempotent
	}

	chunk, chunkExists := file.chunks[chunkIdx]
	if !chunkExists {
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
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := string(fileHandle)
	file, exists := s.files[key]
	if !exists {
		return []cache.Slice{}
	}

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

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return cache.ErrStoreClosed
	}

	key := string(fileHandle)
	file, exists := s.files[key]
	if !exists {
		file = &fileEntry{
			chunks: make(map[uint32]*chunkEntry),
		}
		s.files[key] = file
	}

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

	s.mu.Lock()
	defer s.mu.Unlock()

	key := string(fileHandle)
	file, exists := s.files[key]
	if !exists {
		return cache.ErrFileNotFound
	}

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

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return cache.ErrStoreClosed
	}

	key := string(fileHandle)
	file, exists := s.files[key]
	if !exists {
		file = &fileEntry{
			chunks: make(map[uint32]*chunkEntry),
		}
		s.files[key] = file
	}

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

	s.mu.RLock()
	defer s.mu.RUnlock()

	key := string(fileHandle)
	file, exists := s.files[key]
	if !exists {
		return 0, nil, cache.ErrFileNotFound
	}

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

// ExtendSliceData efficiently extends a slice's data in place.
// Uses Go's append() for amortized O(1) growth on appends.
func (s *Store) ExtendSliceData(ctx context.Context, fileHandle []byte, chunkIdx uint32, sliceID string, data []byte, appendMode bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := string(fileHandle)
	file, exists := s.files[key]
	if !exists {
		return cache.ErrFileNotFound
	}

	chunk, exists := file.chunks[chunkIdx]
	if !exists {
		return cache.ErrChunkNotFound
	}

	for i := range chunk.slices {
		if chunk.slices[i].ID == sliceID {
			oldLen := len(chunk.slices[i].Data)

			if appendMode {
				// Append: use Go's append for amortized O(1) growth
				chunk.slices[i].Data = append(chunk.slices[i].Data, data...)
				chunk.slices[i].Length += uint32(len(data))
			} else {
				// Prepend: need to allocate new slice
				newData := make([]byte, len(data)+len(chunk.slices[i].Data))
				copy(newData, data)
				copy(newData[len(data):], chunk.slices[i].Data)
				chunk.slices[i].Data = newData
				chunk.slices[i].Offset -= uint32(len(data))
				chunk.slices[i].Length += uint32(len(data))
			}

			// Update size tracking
			newLen := len(chunk.slices[i].Data)
			if newLen > oldLen {
				s.totalSize.Add(uint64(newLen - oldLen))
			}

			return nil
		}
	}

	return cache.ErrStoreSliceNotFound
}

// TryExtendAdjacentSlice atomically finds and extends an adjacent pending slice.
// Uses Go's append() for amortized O(1) growth on sequential appends.
func (s *Store) TryExtendAdjacentSlice(ctx context.Context, fileHandle []byte, chunkIdx uint32, offset uint32, data []byte) bool {
	if ctx.Err() != nil {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := string(fileHandle)
	file, exists := s.files[key]
	if !exists {
		return false
	}

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
			oldLen := len(slice.Data)
			slice.Data = append(slice.Data, data...)
			slice.Length += uint32(len(data))
			s.totalSize.Add(uint64(len(slice.Data) - oldLen))
			return true
		}

		// Case 2: Prepending (write ends where slice starts)
		if writeEnd == slice.Offset {
			oldLen := len(slice.Data)
			newData := make([]byte, len(data)+len(slice.Data))
			copy(newData, data)
			copy(newData[len(data):], slice.Data)
			slice.Data = newData
			slice.Offset = offset
			slice.Length += uint32(len(data))
			s.totalSize.Add(uint64(len(newData) - oldLen))
			return true
		}
	}

	return false
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
