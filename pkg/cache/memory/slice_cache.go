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

// Compile-time check that MemorySliceCache implements SliceCache.
var _ cache.SliceCache = (*MemorySliceCache)(nil)

// ============================================================================
// Internal Types
// ============================================================================

// fileEntry holds all cached data for a single file.
type fileEntry struct {
	mu     sync.RWMutex
	chunks map[uint32]*chunkEntry // chunkIndex -> chunkEntry
}

// chunkEntry holds all slices for a single chunk.
type chunkEntry struct {
	slices []*sliceCacheEntry // Ordered by createdAt (newest first)
}

// sliceCacheEntry represents a single cached slice.
type sliceCacheEntry struct {
	id        string
	offset    uint32
	length    uint32
	data      []byte
	state     cache.SliceState
	createdAt time.Time
	blockRefs []cache.BlockRef
}

// ============================================================================
// MemorySliceCache
// ============================================================================

// MemorySliceCache implements SliceCache with in-memory storage.
//
// Thread Safety:
// Two-level locking for efficiency:
//   - c.mu (RWMutex): Protects the files map structure
//   - file.mu (RWMutex): Protects individual file's chunks and slices
//
// This allows concurrent operations on different files without contention.
type MemorySliceCache struct {
	mu        sync.RWMutex
	files     map[string]*fileEntry // fileHandle (as string) -> fileEntry
	closed    bool
	maxSize   uint64
	totalSize atomic.Uint64

	// Statistics
	hits   atomic.Uint64
	misses atomic.Uint64
}

// NewMemorySliceCache creates a new in-memory slice cache.
//
// Parameters:
//   - maxSize: Maximum total cache size in bytes. Use 0 for unlimited.
func NewMemorySliceCache(maxSize uint64) *MemorySliceCache {
	return &MemorySliceCache{
		files:   make(map[string]*fileEntry),
		maxSize: maxSize,
	}
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
func (c *MemorySliceCache) WriteSlice(ctx context.Context, fileHandle []byte, chunkIdx uint32, data []byte, offset uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Validate parameters
	if offset+uint32(len(data)) > cache.ChunkSize {
		return cache.ErrInvalidOffset
	}

	// Get or create file entry
	file, err := c.getOrCreateFile(fileHandle)
	if err != nil {
		return err
	}

	file.mu.Lock()
	defer file.mu.Unlock()

	// Get or create chunk entry
	chunk, exists := file.chunks[chunkIdx]
	if !exists {
		chunk = &chunkEntry{
			slices: make([]*sliceCacheEntry, 0),
		}
		file.chunks[chunkIdx] = chunk
	}

	// Optimization: Try to extend an existing pending slice if this is a sequential write
	if extended := c.tryExtendSlice(chunk, data, offset); extended {
		c.totalSize.Add(uint64(len(data)))
		return nil
	}

	// Create new slice (prepend - newest first)
	slice := &sliceCacheEntry{
		id:        uuid.New().String(),
		offset:    offset,
		length:    uint32(len(data)),
		data:      make([]byte, len(data)),
		state:     cache.SliceStatePending,
		createdAt: time.Now(),
	}
	copy(slice.data, data)

	// Prepend to slices (newest first)
	chunk.slices = append([]*sliceCacheEntry{slice}, chunk.slices...)

	// Update total size
	c.totalSize.Add(uint64(len(data)))

	return nil
}

// tryExtendSlice attempts to extend an existing pending slice with new data.
// Returns true if extension was successful, false if a new slice should be created.
//
// Extension happens when:
//   - The write is adjacent (appending): offset == slice.offset + slice.length
//   - The write is adjacent (prepending): offset + len(data) == slice.offset
//   - The slice is still pending (not flushed)
//
// We do NOT extend if:
//   - The write overlaps (would need complex merge, let newest-wins handle it)
//   - There's a gap (would create sparse data in the slice)
//   - The slice is already flushed
func (c *MemorySliceCache) tryExtendSlice(chunk *chunkEntry, data []byte, offset uint32) bool {
	writeEnd := offset + uint32(len(data))

	for _, slice := range chunk.slices {
		if slice.state != cache.SliceStatePending {
			continue
		}

		sliceEnd := slice.offset + slice.length

		// Case 1: Appending (most common for sequential writes)
		// New write starts exactly where existing slice ends
		if offset == sliceEnd {
			// Extend the slice data
			slice.data = append(slice.data, data...)
			slice.length += uint32(len(data))
			return true
		}

		// Case 2: Prepending (less common, but possible)
		// New write ends exactly where existing slice starts
		if writeEnd == slice.offset {
			// Prepend the data
			newData := make([]byte, len(data)+len(slice.data))
			copy(newData, data)
			copy(newData[len(data):], slice.data)
			slice.data = newData
			slice.offset = offset
			slice.length += uint32(len(data))
			return true
		}

		// Case 3: Overlapping write - don't extend, create new slice
		// The newest-wins semantics will handle this correctly on read
		// and coalescing will merge them properly
	}

	return false
}

// ============================================================================
// Read Operations
// ============================================================================

// ReadSlice reads data from cache with slice merging (newest-wins).
func (c *MemorySliceCache) ReadSlice(ctx context.Context, fileHandle []byte, chunkIdx uint32, offset, length uint32) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	file, err := c.getFile(fileHandle)
	if err != nil {
		c.misses.Add(1)
		return nil, false, err
	}

	file.mu.RLock()
	defer file.mu.RUnlock()

	chunk, exists := file.chunks[chunkIdx]
	if !exists {
		c.misses.Add(1)
		return nil, false, nil
	}

	if len(chunk.slices) == 0 {
		c.misses.Add(1)
		return nil, false, nil
	}

	// Merge slices using newest-wins algorithm
	result := c.mergeSlicesForRead(chunk.slices, offset, length)

	c.hits.Add(1)
	return result, true, nil
}

// mergeSlicesForRead implements the newest-wins slice merge algorithm.
//
// Slices are ordered newest-first, so the first slice covering a byte wins.
// Unwritten regions are filled with zeros.
func (c *MemorySliceCache) mergeSlicesForRead(slices []*sliceCacheEntry, offset, length uint32) []byte {
	result := make([]byte, length)     // Pre-filled with zeros
	covered := make([]bool, length)    // Track which bytes are covered
	coveredCount := uint32(0)

	requestEnd := offset + length

	for _, slice := range slices {
		if coveredCount >= length {
			break // All bytes covered
		}

		sliceEnd := slice.offset + slice.length

		// Check for overlap with requested range
		if slice.offset >= requestEnd || sliceEnd <= offset {
			continue // No overlap
		}

		// Calculate overlap
		overlapStart := max(offset, slice.offset)
		overlapEnd := min(requestEnd, sliceEnd)

		// Copy bytes that aren't already covered
		for i := overlapStart; i < overlapEnd; i++ {
			resultIdx := i - offset
			if !covered[resultIdx] {
				sliceIdx := i - slice.offset
				result[resultIdx] = slice.data[sliceIdx]
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
func (c *MemorySliceCache) GetDirtySlices(ctx context.Context, fileHandle []byte) ([]cache.PendingSlice, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// First coalesce writes
	if err := c.CoalesceWrites(ctx, fileHandle); err != nil {
		return nil, err
	}

	file, err := c.getFile(fileHandle)
	if err != nil {
		return nil, err
	}

	file.mu.RLock()
	defer file.mu.RUnlock()

	var result []cache.PendingSlice

	// Collect all pending slices across all chunks
	for chunkIdx, chunk := range file.chunks {
		for _, slice := range chunk.slices {
			if slice.state == cache.SliceStatePending {
				result = append(result, cache.PendingSlice{
					ID:         slice.id,
					ChunkIndex: chunkIdx,
					Offset:     slice.offset,
					Length:     slice.length,
					Data:       slice.data,
					CreatedAt:  slice.createdAt,
				})
			}
		}
	}

	// Sort by chunk index, then offset
	sort.Slice(result, func(i, j int) bool {
		if result[i].ChunkIndex != result[j].ChunkIndex {
			return result[i].ChunkIndex < result[j].ChunkIndex
		}
		return result[i].Offset < result[j].Offset
	})

	return result, nil
}

// MarkSliceFlushed marks a slice as successfully flushed.
func (c *MemorySliceCache) MarkSliceFlushed(ctx context.Context, fileHandle []byte, sliceID string, blockRefs []cache.BlockRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	file, err := c.getFile(fileHandle)
	if err != nil {
		return err
	}

	file.mu.Lock()
	defer file.mu.Unlock()

	// Find and update the slice
	for _, chunk := range file.chunks {
		for _, slice := range chunk.slices {
			if slice.id == sliceID {
				slice.state = cache.SliceStateFlushed
				slice.blockRefs = blockRefs
				return nil
			}
		}
	}

	return cache.ErrSliceNotFound
}

// ============================================================================
// Write Optimization
// ============================================================================

// CoalesceWrites merges adjacent pending writes into fewer slices.
func (c *MemorySliceCache) CoalesceWrites(ctx context.Context, fileHandle []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	file, err := c.getFile(fileHandle)
	if err != nil {
		return err
	}

	file.mu.Lock()
	defer file.mu.Unlock()

	for _, chunk := range file.chunks {
		c.coalesceChunk(chunk)
	}

	return nil
}

// coalesceChunk merges adjacent pending slices within a chunk.
func (c *MemorySliceCache) coalesceChunk(chunk *chunkEntry) {
	if len(chunk.slices) <= 1 {
		return
	}

	// Separate pending and non-pending slices
	var pending []*sliceCacheEntry
	var other []*sliceCacheEntry

	for _, slice := range chunk.slices {
		if slice.state == cache.SliceStatePending {
			pending = append(pending, slice)
		} else {
			other = append(other, slice)
		}
	}

	if len(pending) <= 1 {
		return // Nothing to coalesce
	}

	// Sort pending slices by offset for coalescing
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].offset < pending[j].offset
	})

	// Merge adjacent or overlapping pending slices
	merged := make([]*sliceCacheEntry, 0)
	var current *sliceCacheEntry

	for _, slice := range pending {
		if current == nil {
			// Start new merged slice
			current = &sliceCacheEntry{
				id:        uuid.New().String(),
				offset:    slice.offset,
				length:    slice.length,
				data:      make([]byte, slice.length),
				state:     cache.SliceStatePending,
				createdAt: time.Now(),
			}
			copy(current.data, slice.data)
			continue
		}

		currentEnd := current.offset + current.length

		if slice.offset <= currentEnd {
			// Adjacent or overlapping - merge
			sliceEnd := slice.offset + slice.length
			newEnd := max(currentEnd, sliceEnd)
			newLength := newEnd - current.offset

			// Extend data buffer if needed
			if newLength > uint32(len(current.data)) {
				newData := make([]byte, newLength)
				copy(newData, current.data)
				current.data = newData
				current.length = newLength
			}

			// Copy slice data (may overwrite - newer data wins)
			dstOffset := slice.offset - current.offset
			copy(current.data[dstOffset:], slice.data)
		} else {
			// Gap - save current and start new
			merged = append(merged, current)
			current = &sliceCacheEntry{
				id:        uuid.New().String(),
				offset:    slice.offset,
				length:    slice.length,
				data:      make([]byte, slice.length),
				state:     cache.SliceStatePending,
				createdAt: time.Now(),
			}
			copy(current.data, slice.data)
		}
	}

	// Don't forget the last one
	if current != nil {
		merged = append(merged, current)
	}

	// Rebuild slices list: merged pending first (newest), then other slices
	chunk.slices = append(merged, other...)
}

// ============================================================================
// Cache Management
// ============================================================================

// Evict removes flushed data for a file.
func (c *MemorySliceCache) Evict(ctx context.Context, fileHandle []byte) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	file, err := c.getFile(fileHandle)
	if err != nil {
		return 0, nil // File not in cache is not an error
	}

	file.mu.Lock()
	defer file.mu.Unlock()

	var evicted uint64

	for _, chunk := range file.chunks {
		remaining := make([]*sliceCacheEntry, 0, len(chunk.slices))
		for _, slice := range chunk.slices {
			if slice.state == cache.SliceStateFlushed {
				evicted += uint64(len(slice.data))
			} else {
				remaining = append(remaining, slice)
			}
		}
		chunk.slices = remaining
	}

	c.totalSize.Add(^(evicted - 1)) // Subtract
	return evicted, nil
}

// EvictAll removes all flushed data from cache.
func (c *MemorySliceCache) EvictAll(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return 0, cache.ErrSliceCacheClosed
	}

	// Get all file handles
	handles := make([][]byte, 0, len(c.files))
	for key := range c.files {
		handles = append(handles, []byte(key))
	}
	c.mu.RUnlock()

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
func (c *MemorySliceCache) Remove(ctx context.Context, fileHandle []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return cache.ErrSliceCacheClosed
	}

	key := string(fileHandle)
	file, exists := c.files[key]
	if !exists {
		return nil // Not an error
	}

	// Calculate size to subtract
	file.mu.Lock()
	var size uint64
	for _, chunk := range file.chunks {
		for _, slice := range chunk.slices {
			size += uint64(len(slice.data))
		}
	}
	file.mu.Unlock()

	delete(c.files, key)
	if size > 0 {
		c.totalSize.Add(^(size - 1))
	}

	return nil
}

// ============================================================================
// File State
// ============================================================================

// HasDirtyData returns true if the file has pending slices.
func (c *MemorySliceCache) HasDirtyData(fileHandle []byte) bool {
	file, err := c.getFile(fileHandle)
	if err != nil {
		return false
	}

	file.mu.RLock()
	defer file.mu.RUnlock()

	for _, chunk := range file.chunks {
		for _, slice := range chunk.slices {
			if slice.state == cache.SliceStatePending {
				return true
			}
		}
	}

	return false
}

// GetFileSize returns the maximum extent of cached data.
func (c *MemorySliceCache) GetFileSize(fileHandle []byte) uint64 {
	file, err := c.getFile(fileHandle)
	if err != nil {
		return 0
	}

	file.mu.RLock()
	defer file.mu.RUnlock()

	var maxChunk uint32
	var maxOffsetInChunk uint32

	for chunkIdx, chunk := range file.chunks {
		for _, slice := range chunk.slices {
			if chunkIdx > maxChunk || (chunkIdx == maxChunk && slice.offset+slice.length > maxOffsetInChunk) {
				maxChunk = chunkIdx
				maxOffsetInChunk = slice.offset + slice.length
			}
		}
	}

	return uint64(maxChunk)*cache.ChunkSize + uint64(maxOffsetInChunk)
}

// ListFiles returns all file handles with cached data.
func (c *MemorySliceCache) ListFiles() [][]byte {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.closed {
		return [][]byte{}
	}

	result := make([][]byte, 0, len(c.files))
	for key := range c.files {
		result = append(result, []byte(key))
	}

	return result
}

// ============================================================================
// Statistics and Lifecycle
// ============================================================================

// GetStats returns current cache statistics.
func (c *MemorySliceCache) GetStats() cache.SliceCacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	stats := cache.SliceCacheStats{
		TotalSize: c.totalSize.Load(),
		MaxSize:   c.maxSize,
		FileCount: len(c.files),
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
	}

	for _, file := range c.files {
		file.mu.RLock()
		for _, chunk := range file.chunks {
			stats.ChunkCount++
			for _, slice := range chunk.slices {
				if slice.state == cache.SliceStatePending {
					stats.PendingSlices++
					stats.DirtySize += uint64(len(slice.data))
				} else {
					stats.FlushedSlices++
				}
			}
		}
		file.mu.RUnlock()
	}

	return stats
}

// Close releases all cache resources.
func (c *MemorySliceCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}

	c.files = nil
	c.totalSize.Store(0)
	c.closed = true

	return nil
}

// ============================================================================
// Helper Methods
// ============================================================================

// getOrCreateFile retrieves or creates a file entry.
func (c *MemorySliceCache) getOrCreateFile(fileHandle []byte) (*fileEntry, error) {
	key := string(fileHandle)

	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return nil, cache.ErrSliceCacheClosed
	}
	file, exists := c.files[key]
	c.mu.RUnlock()

	if exists {
		return file, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, cache.ErrSliceCacheClosed
	}

	// Double-check after acquiring write lock
	file, exists = c.files[key]
	if !exists {
		file = &fileEntry{
			chunks: make(map[uint32]*chunkEntry),
		}
		c.files[key] = file
	}

	return file, nil
}

// getFile retrieves an existing file entry.
func (c *MemorySliceCache) getFile(fileHandle []byte) (*fileEntry, error) {
	key := string(fileHandle)

	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.closed {
		return nil, cache.ErrSliceCacheClosed
	}

	file, exists := c.files[key]
	if !exists {
		return nil, cache.ErrFileNotInCache
	}

	return file, nil
}

