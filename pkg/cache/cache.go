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
//	Cache (business logic + storage)
//	    - In-memory data structures
//	    - Optional mmap backing (future)
//
// See docs/ARCHITECTURE.md for the full Chunk/Slice/Block model.
package cache

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
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
// Internal Types
// ============================================================================

// chunkEntry holds all slices for a single chunk.
type chunkEntry struct {
	slices []Slice // Ordered newest-first (prepended on add)
}

// fileEntry holds all cached data for a single file.
type fileEntry struct {
	mu         sync.RWMutex
	chunks     map[uint32]*chunkEntry // chunkIndex -> chunkEntry
	lastAccess time.Time              // LRU tracking
}

// ============================================================================
// Cache Implementation
// ============================================================================

// Cache is the mandatory cache layer for all content operations.
//
// It understands slices as first-class citizens and stores them directly
// in memory. Optional mmap backing can be enabled for persistence.
//
// Thread Safety:
// Uses two-level locking for efficiency:
//   - globalMu: Protects the files map
//   - per-file mutexes: Protect individual file operations
//
// This allows concurrent operations on different files.
type Cache struct {
	globalMu  sync.RWMutex
	files     map[string]*fileEntry
	maxSize   uint64
	totalSize atomic.Uint64
	closed    bool

	// mmap backing (optional, nil when not enabled)
	mmap *mmapState
}

// New creates a new in-memory cache.
//
// Parameters:
//   - maxSize: Maximum total cache size in bytes. Use 0 for unlimited.
func New(maxSize uint64) *Cache {
	return &Cache{
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

	entry = &fileEntry{
		chunks:     make(map[uint32]*chunkEntry),
		lastAccess: time.Now(),
	}
	c.files[key] = entry
	return entry
}

// touchFile updates the last access time for LRU tracking.
// Must be called with entry.mu held (read or write lock).
func (c *Cache) touchFile(entry *fileEntry) {
	entry.lastAccess = time.Now()
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

	// Enforce maxSize by evicting LRU flushed data if needed
	if c.maxSize > 0 {
		dataLen := uint64(len(data))
		if c.totalSize.Load()+dataLen > c.maxSize {
			c.evictLRUUntilFits(dataLen)
		}
	}

	entry := c.getFileEntry(fileHandle)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	// Update LRU access time
	c.touchFile(entry)

	// Ensure chunk exists
	chunk, exists := entry.chunks[chunkIdx]
	if !exists {
		chunk = &chunkEntry{
			slices: make([]Slice, 0),
		}
		entry.chunks[chunkIdx] = chunk
	}

	// Try to extend an existing adjacent pending slice (sequential write optimization)
	if c.tryExtendAdjacentSlice(chunk, offset, data) {
		return nil
	}

	// Create new slice
	sliceID := uuid.New().String()

	slice := Slice{
		ID:        sliceID,
		Offset:    offset,
		Length:    uint32(len(data)),
		Data:      make([]byte, len(data)),
		State:     SliceStatePending,
		CreatedAt: time.Now(),
	}
	copy(slice.Data, data)

	// Prepend to slices (newest first)
	chunk.slices = append([]Slice{slice}, chunk.slices...)
	c.totalSize.Add(uint64(len(data)))

	// Persist to mmap if enabled
	if c.mmap != nil && c.mmap.enabled {
		// Note: We release the file lock before mmap write to avoid deadlock
		// This is safe because mmap has its own mutex
		entry.mu.Unlock()
		err := c.appendSliceEntry(fileHandle, chunkIdx, &slice)
		entry.mu.Lock() // Re-acquire for deferred unlock
		if err != nil {
			return err
		}
	}

	return nil
}

// tryExtendAdjacentSlice attempts to extend an existing pending slice.
// Uses Go's append() for amortized O(1) growth on sequential appends.
// Returns true if extended, false if no adjacent slice found.
func (c *Cache) tryExtendAdjacentSlice(chunk *chunkEntry, offset uint32, data []byte) bool {
	writeEnd := offset + uint32(len(data))

	for i := range chunk.slices {
		slice := &chunk.slices[i]
		if slice.State != SliceStatePending {
			continue
		}

		sliceEnd := slice.Offset + slice.Length

		// Case 1: Appending (write starts where slice ends)
		if offset == sliceEnd {
			oldLen := len(slice.Data)
			slice.Data = append(slice.Data, data...)
			slice.Length += uint32(len(data))
			c.totalSize.Add(uint64(len(slice.Data) - oldLen))
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
			c.totalSize.Add(uint64(len(newData) - oldLen))
			return true
		}
	}

	return false
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

	entry := c.getFileEntry(fileHandle)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	chunk, exists := entry.chunks[chunkIdx]
	if !exists || len(chunk.slices) == 0 {
		return nil, false, nil
	}

	// Merge slices using newest-wins algorithm
	result, fullyCovered := c.mergeSlicesForRead(chunk.slices, offset, length)
	_ = fullyCovered // Not used for reads - sparse files may have gaps

	return result, true, nil
}

// mergeSlicesForRead implements the newest-wins slice merge algorithm.
// Returns the merged data and whether the entire range is covered.
func (c *Cache) mergeSlicesForRead(slices []Slice, offset, length uint32) ([]byte, bool) {
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

	return result, coveredCount == length
}

// IsRangeCovered checks if a byte range is fully covered by cached slices.
// This is used by the flusher to determine if a block is ready for upload.
func (c *Cache) IsRangeCovered(ctx context.Context, fileHandle []byte, chunkIdx uint32, offset, length uint32) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return false, ErrCacheClosed
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(fileHandle)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	chunk, exists := entry.chunks[chunkIdx]
	if !exists || len(chunk.slices) == 0 {
		return false, nil
	}

	// Check coverage without allocating result buffer
	coveredCount := uint32(0)
	requestEnd := offset + length

	for _, slice := range chunk.slices {
		if coveredCount >= length {
			break
		}

		sliceEnd := slice.Offset + slice.Length

		if slice.Offset >= requestEnd || sliceEnd <= offset {
			continue
		}

		overlapStart := max(offset, slice.Offset)
		overlapEnd := min(requestEnd, sliceEnd)
		coveredCount += overlapEnd - overlapStart
	}

	return coveredCount >= length, nil
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

	entry := c.getFileEntry(fileHandle)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	if len(entry.chunks) == 0 {
		return nil, ErrFileNotInCache
	}

	var result []PendingSlice

	for chunkIdx, chunk := range entry.chunks {
		for _, slice := range chunk.slices {
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

	// Find the slice
	for _, chunk := range entry.chunks {
		for i := range chunk.slices {
			if chunk.slices[i].ID == sliceID {
				chunk.slices[i].State = SliceStateFlushed
				if blockRefs != nil {
					chunk.slices[i].BlockRefs = make([]BlockRef, len(blockRefs))
					copy(chunk.slices[i].BlockRefs, blockRefs)
				}
				return nil
			}
		}
	}

	return ErrSliceNotFound
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

	entry := c.getFileEntry(fileHandle)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	if len(entry.chunks) == 0 {
		return ErrFileNotInCache
	}

	for chunkIdx := range entry.chunks {
		if err := c.coalesceChunk(entry.chunks[chunkIdx]); err != nil {
			return err
		}
	}

	return nil
}

// coalesceChunk merges adjacent pending slices within a chunk.
func (c *Cache) coalesceChunk(chunk *chunkEntry) error {
	if len(chunk.slices) <= 1 {
		return nil
	}

	var pending []Slice
	var other []Slice

	for _, slice := range chunk.slices {
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

	chunk.slices = append(merged, other...)
	return nil
}

// ============================================================================
// Cache Management
// ============================================================================

// lruEntry holds file handle and last access time for LRU sorting.
type lruEntry struct {
	handle     string
	lastAccess time.Time
}

// evictLRUUntilFits evicts flushed slices from least recently used files
// until the cache has room for neededBytes.
// Only flushed slices can be evicted - dirty (pending/uploading) slices are protected.
func (c *Cache) evictLRUUntilFits(neededBytes uint64) {
	if c.maxSize == 0 {
		return // Unlimited cache, no eviction needed
	}
	targetSize := c.maxSize - neededBytes
	c.evictLRUToTarget(targetSize)
}

// evictLRUToTarget evicts flushed slices from LRU files until size <= target.
func (c *Cache) evictLRUToTarget(targetSize uint64) {
	// Collect files with their access times
	c.globalMu.RLock()
	entries := make([]lruEntry, 0, len(c.files))
	for handle, entry := range c.files {
		entry.mu.RLock()
		entries = append(entries, lruEntry{
			handle:     handle,
			lastAccess: entry.lastAccess,
		})
		entry.mu.RUnlock()
	}
	c.globalMu.RUnlock()

	// Sort by last access (oldest first)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].lastAccess.Before(entries[j].lastAccess)
	})

	// Evict flushed slices from LRU files until we reach target size
	for _, lru := range entries {
		currentSize := c.totalSize.Load()
		if currentSize <= targetSize {
			break
		}

		// Evict flushed slices from this file
		c.evictFlushedFromFile(lru.handle)
	}
}

// evictFlushedFromFile removes flushed slices from a specific file.
// This is an internal helper that doesn't check context.
func (c *Cache) evictFlushedFromFile(handle string) uint64 {
	c.globalMu.RLock()
	entry, exists := c.files[handle]
	c.globalMu.RUnlock()

	if !exists {
		return 0
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()

	var evicted uint64

	for chunkIdx, chunk := range entry.chunks {
		var remaining []Slice
		for _, slice := range chunk.slices {
			if slice.State == SliceStateFlushed {
				evicted += uint64(len(slice.Data))
				c.totalSize.Add(^(uint64(len(slice.Data)) - 1)) // Subtract
			} else {
				remaining = append(remaining, slice)
			}
		}

		if len(remaining) == 0 {
			delete(entry.chunks, chunkIdx)
		} else if len(remaining) != len(chunk.slices) {
			chunk.slices = remaining
		}
	}

	return evicted
}

// EvictLRU evicts flushed slices from least recently used files to free up space.
// Returns the total bytes evicted. Only flushed slices are evicted - dirty data is protected.
// The targetFreeBytes parameter specifies how much space to free.
func (c *Cache) EvictLRU(ctx context.Context, targetFreeBytes uint64) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return 0, ErrCacheClosed
	}
	c.globalMu.RUnlock()

	startSize := c.totalSize.Load()

	// For explicit eviction, calculate target size based on current size
	if startSize > targetFreeBytes {
		c.evictLRUToTarget(startSize - targetFreeBytes)
	} else {
		// Need to evict everything possible
		c.evictLRUToTarget(0)
	}

	endSize := c.totalSize.Load()
	if startSize > endSize {
		return startSize - endSize, nil
	}
	return 0, nil
}

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

	entry := c.getFileEntry(fileHandle)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	var evicted uint64

	for chunkIdx, chunk := range entry.chunks {
		var remaining []Slice
		for _, slice := range chunk.slices {
			if slice.State == SliceStateFlushed {
				evicted += uint64(len(slice.Data))
				c.totalSize.Add(^(uint64(len(slice.Data)) - 1)) // Subtract
			} else {
				remaining = append(remaining, slice)
			}
		}

		if len(remaining) == 0 {
			delete(entry.chunks, chunkIdx)
		} else if len(remaining) != len(chunk.slices) {
			chunk.slices = remaining
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
	handles := make([][]byte, 0, len(c.files))
	for k := range c.files {
		handles = append(handles, []byte(k))
	}
	c.globalMu.RUnlock()

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

	c.globalMu.Lock()
	defer c.globalMu.Unlock()

	if c.closed {
		return ErrCacheClosed
	}

	key := string(fileHandle)
	entry, exists := c.files[key]
	if !exists {
		return nil // Idempotent
	}

	// Calculate size to subtract
	var size uint64
	for _, chunk := range entry.chunks {
		for _, slice := range chunk.slices {
			size += uint64(len(slice.Data))
		}
	}

	delete(c.files, key)

	if size > 0 {
		c.totalSize.Add(^(size - 1)) // Subtract
	}

	// Persist removal to mmap if enabled
	if c.mmap != nil && c.mmap.enabled {
		if err := c.appendRemoveEntry(fileHandle); err != nil {
			return err
		}
	}

	return nil
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

	entry := c.getFileEntry(fileHandle)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	if len(entry.chunks) == 0 {
		return nil
	}

	currentSize := c.getFileSizeUnlocked(entry)
	if newSize >= currentSize {
		return nil
	}

	newEndChunk := ChunkIndexForOffset(newSize)
	newOffsetInEndChunk := OffsetWithinChunk(newSize)

	for chunkIdx, chunk := range entry.chunks {
		if chunkIdx > newEndChunk {
			// Calculate size to subtract
			var size uint64
			for _, slice := range chunk.slices {
				size += uint64(len(slice.Data))
			}
			if size > 0 {
				c.totalSize.Add(^(size - 1)) // Subtract
			}
			delete(entry.chunks, chunkIdx)
		} else if chunkIdx == newEndChunk {
			var newSlices []Slice
			var removedSize uint64

			for _, slice := range chunk.slices {
				sliceEnd := slice.Offset + slice.Length

				if slice.Offset >= newOffsetInEndChunk {
					removedSize += uint64(len(slice.Data))
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
				removedSize += uint64(slice.Length - newLength)
				newSlices = append(newSlices, truncatedSlice)
			}

			if removedSize > 0 {
				c.totalSize.Add(^(removedSize - 1)) // Subtract
			}

			if len(newSlices) == 0 {
				delete(entry.chunks, chunkIdx)
			} else {
				chunk.slices = newSlices
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

	entry := c.getFileEntry(fileHandle)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	for _, chunk := range entry.chunks {
		for _, slice := range chunk.slices {
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

	entry := c.getFileEntry(fileHandle)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	return c.getFileSizeUnlocked(entry)
}

func (c *Cache) getFileSizeUnlocked(entry *fileEntry) uint64 {
	var maxChunk uint32
	var maxOffsetInChunk uint32

	for chunkIdx, chunk := range entry.chunks {
		for _, slice := range chunk.slices {
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
	defer c.globalMu.RUnlock()

	if c.closed {
		return [][]byte{}
	}

	result := make([][]byte, 0, len(c.files))
	for key := range c.files {
		result = append(result, []byte(key))
	}

	return result
}

// GetTotalSize returns the total bytes stored in the cache.
func (c *Cache) GetTotalSize() uint64 {
	return c.totalSize.Load()
}

// Stats returns current cache statistics for observability.
func (c *Cache) Stats() Stats {
	c.globalMu.RLock()
	defer c.globalMu.RUnlock()

	if c.closed {
		return Stats{}
	}

	stats := Stats{
		TotalSize: c.totalSize.Load(),
		MaxSize:   c.maxSize,
		FileCount: len(c.files),
	}

	for _, entry := range c.files {
		entry.mu.RLock()
		for _, chunk := range entry.chunks {
			for _, slice := range chunk.slices {
				stats.SliceCount++
				size := uint64(len(slice.Data))
				if slice.State == SliceStateFlushed {
					stats.FlushedBytes += size
				} else {
					stats.DirtyBytes += size
				}
			}
		}
		entry.mu.RUnlock()
	}

	return stats
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
	c.totalSize.Store(0)

	// Close mmap if enabled
	if c.mmap != nil && c.mmap.enabled {
		if err := c.closeMmap(); err != nil {
			return err
		}
	}

	return nil
}
