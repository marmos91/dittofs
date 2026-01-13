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
//	Cache (business logic)
//	    ↓
//	Store (interface, persistence)
//	    ↓
//	store/memory/ or store/filesystem/ (implementations)
//
// See docs/ARCHITECTURE.md for the full Chunk/Slice/Block model.
package cache

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

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

// ============================================================================
// Cache Implementation
// ============================================================================

// fileEntry holds per-file data with a mutex for concurrent access.
type fileEntry struct {
	mu sync.RWMutex
}

// Cache is the mandatory cache layer for all content operations.
//
// It understands slices as first-class citizens but is agnostic to the
// underlying storage backend (S3, filesystem, memory, Redis).
//
// Thread Safety:
// Uses two-level locking for efficiency:
//   - globalMu: Protects the files map and store
//   - per-file mutexes: Protect individual file operations
//
// This allows concurrent operations on different files.
type Cache struct {
	globalMu sync.RWMutex
	store    Store
	files    map[string]*fileEntry
	maxSize  uint64
	closed   bool
}

// NewWithStore creates a cache with a custom store.
//
// Parameters:
//   - store: The persistence layer for slices
//   - maxSize: Maximum total cache size in bytes. Use 0 for unlimited.
func NewWithStore(store Store, maxSize uint64) *Cache {
	return &Cache{
		store:   store,
		files:   make(map[string]*fileEntry),
		maxSize: maxSize,
	}
}

// getFileEntry returns or creates a file entry with its mutex.
func (c *Cache) getFileEntry(fileHandle []byte) *fileEntry {
	key := string(fileHandle)

	c.globalMu.RLock()
	entry, exists := c.files[key]
	c.globalMu.RUnlock()

	if exists {
		return entry
	}

	c.globalMu.Lock()
	defer c.globalMu.Unlock()

	// Double-check after acquiring write lock
	if entry, exists = c.files[key]; exists {
		return entry
	}

	// Create the file entry in the Store
	_ = c.store.CreateFile(context.Background(), fileHandle)

	entry = &fileEntry{}
	c.files[key] = entry
	return entry
}

// ============================================================================
// Write Operations
// ============================================================================

// WriteSlice writes a slice to the cache.
//
// Optimization: If the write is adjacent to an existing pending slice (sequential write),
// we extend that slice instead of creating a new one. This is critical for performance
// since NFS clients write in 16KB-32KB chunks, so a 10MB file = 320 writes.
// Without this optimization, we'd create 320 slices instead of 1.
func (c *Cache) WriteSlice(ctx context.Context, fileHandle []byte, chunkIdx uint32, data []byte, offset uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return ErrCacheClosed
	}
	c.globalMu.RUnlock()

	// Validate parameters
	if offset+uint32(len(data)) > ChunkSize {
		return ErrInvalidOffset
	}

	entry := c.getFileEntry(fileHandle)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	// Try to extend an existing adjacent pending slice (sequential write optimization)
	if c.store.TryExtendAdjacentSlice(ctx, fileHandle, chunkIdx, offset, data) {
		return nil
	}

	// Create new slice
	slice := Slice{
		ID:        uuid.New().String(),
		Offset:    offset,
		Length:    uint32(len(data)),
		Data:      make([]byte, len(data)),
		State:     SliceStatePending,
		CreatedAt: time.Now(),
	}
	copy(slice.Data, data)

	return c.store.AddSlice(ctx, fileHandle, chunkIdx, slice)
}

// ============================================================================
// Read Operations
// ============================================================================

// ReadSlice reads data from cache with slice merging (newest-wins).
func (c *Cache) ReadSlice(ctx context.Context, fileHandle []byte, chunkIdx uint32, offset, length uint32) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return nil, false, ErrCacheClosed
	}
	c.globalMu.RUnlock()

	if !c.store.FileExists(ctx, fileHandle) {
		return nil, false, ErrFileNotInCache
	}

	entry := c.getFileEntry(fileHandle)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	slices := c.store.GetSlices(ctx, fileHandle, chunkIdx)
	if len(slices) == 0 {
		return nil, false, nil
	}

	// Merge slices using newest-wins algorithm
	result := c.mergeSlicesForRead(slices, offset, length)

	return result, true, nil
}

// mergeSlicesForRead implements the newest-wins slice merge algorithm.
func (c *Cache) mergeSlicesForRead(slices []Slice, offset, length uint32) []byte {
	result := make([]byte, length)
	covered := make([]bool, length)
	coveredCount := uint32(0)

	requestEnd := offset + length

	for _, slice := range slices {
		if coveredCount >= length {
			break
		}

		sliceEnd := slice.Offset + slice.Length

		if slice.Offset >= requestEnd || sliceEnd <= offset {
			continue
		}

		overlapStart := max(offset, slice.Offset)
		overlapEnd := min(requestEnd, sliceEnd)

		for i := overlapStart; i < overlapEnd; i++ {
			resultIdx := i - offset
			if !covered[resultIdx] {
				sliceIdx := i - slice.Offset
				result[resultIdx] = slice.Data[sliceIdx]
				covered[resultIdx] = true
				coveredCount++
			}
		}
	}

	return result
}

// ============================================================================
// Flush Coordination
// ============================================================================

// GetDirtySlices returns all pending slices for a file.
func (c *Cache) GetDirtySlices(ctx context.Context, fileHandle []byte) ([]PendingSlice, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return nil, ErrCacheClosed
	}
	c.globalMu.RUnlock()

	if err := c.CoalesceWrites(ctx, fileHandle); err != nil && err != ErrFileNotInCache {
		return nil, err
	}

	if !c.store.FileExists(ctx, fileHandle) {
		return nil, ErrFileNotInCache
	}

	entry := c.getFileEntry(fileHandle)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	var result []PendingSlice

	chunkIndices := c.store.GetChunkIndices(ctx, fileHandle)
	for _, chunkIdx := range chunkIndices {
		slices := c.store.GetSlices(ctx, fileHandle, chunkIdx)
		for _, slice := range slices {
			if slice.State == SliceStatePending {
				result = append(result, PendingSlice{
					ID:         slice.ID,
					ChunkIndex: chunkIdx,
					Offset:     slice.Offset,
					Length:     slice.Length,
					Data:       slice.Data,
					CreatedAt:  slice.CreatedAt,
				})
			}
		}
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].ChunkIndex != result[j].ChunkIndex {
			return result[i].ChunkIndex < result[j].ChunkIndex
		}
		return result[i].Offset < result[j].Offset
	})

	return result, nil
}

// MarkSliceFlushed marks a slice as successfully flushed.
func (c *Cache) MarkSliceFlushed(ctx context.Context, fileHandle []byte, sliceID string, blockRefs []BlockRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return ErrCacheClosed
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(fileHandle)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	chunkIdx, _, err := c.store.FindSlice(ctx, fileHandle, sliceID)
	if err != nil {
		if err == ErrStoreSliceNotFound {
			return ErrSliceNotFound
		}
		if err == ErrFileNotFound {
			return ErrFileNotInCache
		}
		return err
	}

	state := SliceStateFlushed
	update := SliceUpdate{
		State:     &state,
		BlockRefs: blockRefs,
	}

	return c.store.UpdateSlice(ctx, fileHandle, chunkIdx, sliceID, update)
}

// ============================================================================
// Write Optimization
// ============================================================================

// CoalesceWrites merges adjacent pending writes into fewer slices.
func (c *Cache) CoalesceWrites(ctx context.Context, fileHandle []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return ErrCacheClosed
	}
	c.globalMu.RUnlock()

	if !c.store.FileExists(ctx, fileHandle) {
		return ErrFileNotInCache
	}

	entry := c.getFileEntry(fileHandle)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	chunkIndices := c.store.GetChunkIndices(ctx, fileHandle)
	for _, chunkIdx := range chunkIndices {
		if err := c.coalesceChunk(ctx, fileHandle, chunkIdx); err != nil {
			return err
		}
	}

	return nil
}

// coalesceChunk merges adjacent pending slices within a chunk.
func (c *Cache) coalesceChunk(ctx context.Context, fileHandle []byte, chunkIdx uint32) error {
	slices := c.store.GetSlices(ctx, fileHandle, chunkIdx)
	if len(slices) <= 1 {
		return nil
	}

	var pending []Slice
	var other []Slice

	for _, slice := range slices {
		if slice.State == SliceStatePending {
			pending = append(pending, slice)
		} else {
			other = append(other, slice)
		}
	}

	if len(pending) <= 1 {
		return nil
	}

	sort.Slice(pending, func(i, j int) bool {
		return pending[i].Offset < pending[j].Offset
	})

	merged := make([]Slice, 0)
	var current *Slice

	for _, slice := range pending {
		if current == nil {
			newSlice := Slice{
				ID:        uuid.New().String(),
				Offset:    slice.Offset,
				Length:    slice.Length,
				Data:      make([]byte, slice.Length),
				State:     SliceStatePending,
				CreatedAt: time.Now(),
			}
			copy(newSlice.Data, slice.Data)
			current = &newSlice
			continue
		}

		currentEnd := current.Offset + current.Length

		if slice.Offset <= currentEnd {
			sliceEnd := slice.Offset + slice.Length
			newEnd := max(currentEnd, sliceEnd)
			newLength := newEnd - current.Offset

			if newLength > uint32(len(current.Data)) {
				newData := make([]byte, newLength)
				copy(newData, current.Data)
				current.Data = newData
				current.Length = newLength
			}

			dstOffset := slice.Offset - current.Offset
			copy(current.Data[dstOffset:], slice.Data)
		} else {
			merged = append(merged, *current)
			newSlice := Slice{
				ID:        uuid.New().String(),
				Offset:    slice.Offset,
				Length:    slice.Length,
				Data:      make([]byte, slice.Length),
				State:     SliceStatePending,
				CreatedAt: time.Now(),
			}
			copy(newSlice.Data, slice.Data)
			current = &newSlice
		}
	}

	if current != nil {
		merged = append(merged, *current)
	}

	newSlices := append(merged, other...)

	return c.store.SetSlices(ctx, fileHandle, chunkIdx, newSlices)
}

// ============================================================================
// Cache Management
// ============================================================================

// Evict removes flushed data for a file.
func (c *Cache) Evict(ctx context.Context, fileHandle []byte) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return 0, ErrCacheClosed
	}
	c.globalMu.RUnlock()

	if !c.store.FileExists(ctx, fileHandle) {
		return 0, nil
	}

	entry := c.getFileEntry(fileHandle)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	var evicted uint64

	chunkIndices := c.store.GetChunkIndices(ctx, fileHandle)
	for _, chunkIdx := range chunkIndices {
		slices := c.store.GetSlices(ctx, fileHandle, chunkIdx)

		var remaining []Slice
		for _, slice := range slices {
			if slice.State == SliceStateFlushed {
				evicted += uint64(len(slice.Data))
			} else {
				remaining = append(remaining, slice)
			}
		}

		if len(remaining) == 0 {
			_ = c.store.RemoveChunk(ctx, fileHandle, chunkIdx)
		} else if len(remaining) != len(slices) {
			_ = c.store.SetSlices(ctx, fileHandle, chunkIdx, remaining)
		}
	}

	return evicted, nil
}

// EvictAll removes all flushed data from cache.
func (c *Cache) EvictAll(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return 0, ErrCacheClosed
	}
	c.globalMu.RUnlock()

	handles := c.store.ListFiles(ctx)

	var total uint64
	for _, handle := range handles {
		evicted, err := c.Evict(ctx, handle)
		if err != nil {
			return total, err
		}
		total += evicted
	}

	return total, nil
}

// Remove completely removes all cached data for a file.
func (c *Cache) Remove(ctx context.Context, fileHandle []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return ErrCacheClosed
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(fileHandle)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	return c.store.RemoveFile(ctx, fileHandle)
}

// Truncate changes the size of cached data for a file.
func (c *Cache) Truncate(ctx context.Context, fileHandle []byte, newSize uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return ErrCacheClosed
	}
	c.globalMu.RUnlock()

	if !c.store.FileExists(ctx, fileHandle) {
		return nil
	}

	entry := c.getFileEntry(fileHandle)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	currentSize := c.getFileSizeUnlocked(ctx, fileHandle)
	if newSize >= currentSize {
		return nil
	}

	newEndChunk := ChunkIndexForOffset(newSize)
	newOffsetInEndChunk := OffsetWithinChunk(newSize)

	chunkIndices := c.store.GetChunkIndices(ctx, fileHandle)
	for _, chunkIdx := range chunkIndices {
		if chunkIdx > newEndChunk {
			_ = c.store.RemoveChunk(ctx, fileHandle, chunkIdx)
		} else if chunkIdx == newEndChunk {
			slices := c.store.GetSlices(ctx, fileHandle, chunkIdx)
			var newSlices []Slice

			for _, slice := range slices {
				sliceEnd := slice.Offset + slice.Length

				if slice.Offset >= newOffsetInEndChunk {
					continue
				}

				if sliceEnd <= newOffsetInEndChunk {
					newSlices = append(newSlices, slice)
					continue
				}

				newLength := newOffsetInEndChunk - slice.Offset
				truncatedSlice := slice
				truncatedSlice.Data = slice.Data[:newLength]
				truncatedSlice.Length = newLength
				newSlices = append(newSlices, truncatedSlice)
			}

			if len(newSlices) == 0 {
				_ = c.store.RemoveChunk(ctx, fileHandle, chunkIdx)
			} else {
				_ = c.store.SetSlices(ctx, fileHandle, chunkIdx, newSlices)
			}
		}
	}

	return nil
}

// ============================================================================
// File State
// ============================================================================

// HasDirtyData returns true if the file has pending slices.
func (c *Cache) HasDirtyData(fileHandle []byte) bool {
	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return false
	}
	c.globalMu.RUnlock()

	ctx := context.Background()
	if !c.store.FileExists(ctx, fileHandle) {
		return false
	}

	entry := c.getFileEntry(fileHandle)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	chunkIndices := c.store.GetChunkIndices(ctx, fileHandle)
	for _, chunkIdx := range chunkIndices {
		slices := c.store.GetSlices(ctx, fileHandle, chunkIdx)
		for _, slice := range slices {
			if slice.State == SliceStatePending {
				return true
			}
		}
	}

	return false
}

// GetFileSize returns the maximum extent of cached data.
func (c *Cache) GetFileSize(fileHandle []byte) uint64 {
	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return 0
	}
	c.globalMu.RUnlock()

	ctx := context.Background()
	if !c.store.FileExists(ctx, fileHandle) {
		return 0
	}

	entry := c.getFileEntry(fileHandle)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	return c.getFileSizeUnlocked(ctx, fileHandle)
}

func (c *Cache) getFileSizeUnlocked(ctx context.Context, fileHandle []byte) uint64 {
	var maxChunk uint32
	var maxOffsetInChunk uint32

	chunkIndices := c.store.GetChunkIndices(ctx, fileHandle)
	for _, chunkIdx := range chunkIndices {
		slices := c.store.GetSlices(ctx, fileHandle, chunkIdx)
		for _, slice := range slices {
			if chunkIdx > maxChunk || (chunkIdx == maxChunk && slice.Offset+slice.Length > maxOffsetInChunk) {
				maxChunk = chunkIdx
				maxOffsetInChunk = slice.Offset + slice.Length
			}
		}
	}

	return uint64(maxChunk)*ChunkSize + uint64(maxOffsetInChunk)
}

// ListFiles returns all file handles with cached data.
func (c *Cache) ListFiles() [][]byte {
	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return [][]byte{}
	}
	c.globalMu.RUnlock()

	return c.store.ListFiles(context.Background())
}

// ============================================================================
// Lifecycle
// ============================================================================

// Close releases all cache resources.
func (c *Cache) Close() error {
	c.globalMu.Lock()
	defer c.globalMu.Unlock()

	if c.closed {
		return nil
	}

	c.closed = true
	c.files = nil

	return c.store.Close()
}
