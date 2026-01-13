package memory

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/cache"
)

// Compile-time check that MemoryCache implements cache.Cache.
var _ cache.Cache = (*MemoryCache)(nil)

// ============================================================================
// MemoryCache
// ============================================================================

// MemoryCache implements cache.Cache using a memory store for persistence.
//
// This implementation keeps business logic (slice merging, coalescing,
// sequential write optimization) in the cache layer while delegating
// persistence to a cache.Store implementation.
//
// Thread Safety:
// All methods are safe for concurrent use. The underlying store handles
// thread safety for persistence operations.
type MemoryCache struct {
	store   cache.Store
	maxSize uint64
}

// NewCache creates a new in-memory cache.
//
// Parameters:
//   - maxSize: Maximum total cache size in bytes. Use 0 for unlimited.
func NewCache(maxSize uint64) *MemoryCache {
	return &MemoryCache{
		store:   New(),
		maxSize: maxSize,
	}
}

// NewCacheWithStore creates a cache with a custom store.
// This is useful for testing or using different store implementations.
func NewCacheWithStore(st cache.Store, maxSize uint64) *MemoryCache {
	return &MemoryCache{
		store:   st,
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
func (c *MemoryCache) WriteSlice(ctx context.Context, fileHandle []byte, chunkIdx uint32, data []byte, offset uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if c.store.IsClosed() {
		return cache.ErrCacheClosed
	}

	// Validate parameters
	if offset+uint32(len(data)) > cache.ChunkSize {
		return cache.ErrInvalidOffset
	}

	// Try to extend an existing adjacent pending slice atomically (sequential write optimization).
	// This uses the store's ExtendAdjacentSlice to avoid TOCTOU race conditions.
	if c.store.ExtendAdjacentSlice(ctx, fileHandle, chunkIdx, offset, data) {
		return nil
	}

	// Create new slice
	slice := cache.Slice{
		ID:        uuid.New().String(),
		Offset:    offset,
		Length:    uint32(len(data)),
		Data:      make([]byte, len(data)),
		State:     cache.SliceStatePending,
		CreatedAt: time.Now(),
	}
	copy(slice.Data, data)

	return c.store.AddSlice(ctx, fileHandle, chunkIdx, slice)
}

// ============================================================================
// Read Operations
// ============================================================================

// ReadSlice reads data from cache with slice merging (newest-wins).
func (c *MemoryCache) ReadSlice(ctx context.Context, fileHandle []byte, chunkIdx uint32, offset, length uint32) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	if c.store.IsClosed() {
		return nil, false, cache.ErrCacheClosed
	}

	if !c.store.FileExists(ctx, fileHandle) {
		return nil, false, cache.ErrFileNotInCache
	}

	slices := c.store.GetSlices(ctx, fileHandle, chunkIdx)
	if len(slices) == 0 {
		return nil, false, nil
	}

	// Merge slices using newest-wins algorithm
	result := c.mergeSlicesForRead(slices, offset, length)

	return result, true, nil
}

// mergeSlicesForRead implements the newest-wins slice merge algorithm.
//
// Slices are ordered newest-first, so the first slice covering a byte wins.
// Unwritten regions are filled with zeros.
func (c *MemoryCache) mergeSlicesForRead(slices []cache.Slice, offset, length uint32) []byte {
	result := make([]byte, length)  // Pre-filled with zeros
	covered := make([]bool, length) // Track which bytes are covered
	coveredCount := uint32(0)

	requestEnd := offset + length

	for _, slice := range slices {
		if coveredCount >= length {
			break // All bytes covered
		}

		sliceEnd := slice.Offset + slice.Length

		// Check for overlap with requested range
		if slice.Offset >= requestEnd || sliceEnd <= offset {
			continue // No overlap
		}

		// Calculate overlap
		overlapStart := max(offset, slice.Offset)
		overlapEnd := min(requestEnd, sliceEnd)

		// Copy bytes that aren't already covered
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
func (c *MemoryCache) GetDirtySlices(ctx context.Context, fileHandle []byte) ([]cache.PendingSlice, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if c.store.IsClosed() {
		return nil, cache.ErrCacheClosed
	}

	// First coalesce writes
	if err := c.CoalesceWrites(ctx, fileHandle); err != nil && err != cache.ErrFileNotInCache {
		return nil, err
	}

	if !c.store.FileExists(ctx, fileHandle) {
		return nil, cache.ErrFileNotInCache
	}

	var result []cache.PendingSlice

	// Collect all pending slices across all chunks
	chunkIndices := c.store.GetChunkIndices(ctx, fileHandle)
	for _, chunkIdx := range chunkIndices {
		slices := c.store.GetSlices(ctx, fileHandle, chunkIdx)
		for _, slice := range slices {
			if slice.State == cache.SliceStatePending {
				result = append(result, cache.PendingSlice{
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
func (c *MemoryCache) MarkSliceFlushed(ctx context.Context, fileHandle []byte, sliceID string, blockRefs []cache.BlockRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if c.store.IsClosed() {
		return cache.ErrCacheClosed
	}

	// Find the slice across all chunks
	chunkIdx, _, err := c.store.FindSlice(ctx, fileHandle, sliceID)
	if err != nil {
		if err == cache.ErrStoreSliceNotFound {
			return cache.ErrSliceNotFound
		}
		if err == cache.ErrFileNotFound {
			return cache.ErrFileNotInCache
		}
		return err
	}

	state := cache.SliceStateFlushed
	update := cache.SliceUpdate{
		State:     &state,
		BlockRefs: blockRefs,
	}

	return c.store.UpdateSlice(ctx, fileHandle, chunkIdx, sliceID, update)
}

// ============================================================================
// Write Optimization
// ============================================================================

// CoalesceWrites merges adjacent pending writes into fewer slices.
func (c *MemoryCache) CoalesceWrites(ctx context.Context, fileHandle []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if c.store.IsClosed() {
		return cache.ErrCacheClosed
	}

	if !c.store.FileExists(ctx, fileHandle) {
		return cache.ErrFileNotInCache
	}

	chunkIndices := c.store.GetChunkIndices(ctx, fileHandle)
	for _, chunkIdx := range chunkIndices {
		if err := c.coalesceChunk(ctx, fileHandle, chunkIdx); err != nil {
			return err
		}
	}

	return nil
}

// coalesceChunk merges adjacent pending slices within a chunk.
// Delegates to the store's implementation which holds the lock during the entire operation.
func (c *MemoryCache) coalesceChunk(ctx context.Context, fileHandle []byte, chunkIdx uint32) error {
	return c.store.CoalesceChunk(ctx, fileHandle, chunkIdx)
}

// ============================================================================
// Cache Management
// ============================================================================

// Evict removes flushed data for a file.
func (c *MemoryCache) Evict(ctx context.Context, fileHandle []byte) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	if c.store.IsClosed() {
		return 0, cache.ErrCacheClosed
	}

	if !c.store.FileExists(ctx, fileHandle) {
		return 0, nil // Not an error
	}

	var evicted uint64

	chunkIndices := c.store.GetChunkIndices(ctx, fileHandle)
	for _, chunkIdx := range chunkIndices {
		slices := c.store.GetSlices(ctx, fileHandle, chunkIdx)

		var remaining []cache.Slice
		for _, slice := range slices {
			if slice.State == cache.SliceStateFlushed {
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
func (c *MemoryCache) EvictAll(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	if c.store.IsClosed() {
		return 0, cache.ErrCacheClosed
	}

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
func (c *MemoryCache) Remove(ctx context.Context, fileHandle []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if c.store.IsClosed() {
		return cache.ErrCacheClosed
	}

	return c.store.RemoveFile(ctx, fileHandle)
}

// Truncate changes the size of cached data for a file.
func (c *MemoryCache) Truncate(ctx context.Context, fileHandle []byte, newSize uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if c.store.IsClosed() {
		return cache.ErrCacheClosed
	}

	if !c.store.FileExists(ctx, fileHandle) {
		return nil // Not an error
	}

	currentSize := c.GetFileSize(fileHandle)
	if newSize >= currentSize {
		// Extending or no-op - nothing to do (sparse file semantics)
		return nil
	}

	// Calculate which chunks are affected
	newEndChunk := cache.ChunkIndexForOffset(newSize)
	newOffsetInEndChunk := cache.OffsetWithinChunk(newSize)

	// Remove chunks beyond the new size
	chunkIndices := c.store.GetChunkIndices(ctx, fileHandle)
	for _, chunkIdx := range chunkIndices {
		if chunkIdx > newEndChunk {
			// Remove entire chunk
			_ = c.store.RemoveChunk(ctx, fileHandle, chunkIdx)
		} else if chunkIdx == newEndChunk {
			// Truncate slices in the last chunk
			slices := c.store.GetSlices(ctx, fileHandle, chunkIdx)
			var newSlices []cache.Slice

			for _, slice := range slices {
				sliceEnd := slice.Offset + slice.Length

				if slice.Offset >= newOffsetInEndChunk {
					// Slice starts at or after truncation point - remove entirely
					continue
				}

				if sliceEnd <= newOffsetInEndChunk {
					// Slice ends before truncation point - keep entirely
					newSlices = append(newSlices, slice)
					continue
				}

				// Slice spans truncation point - truncate it
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
func (c *MemoryCache) HasDirtyData(fileHandle []byte) bool {
	if c.store.IsClosed() {
		return false
	}

	ctx := context.Background()
	if !c.store.FileExists(ctx, fileHandle) {
		return false
	}

	chunkIndices := c.store.GetChunkIndices(ctx, fileHandle)
	for _, chunkIdx := range chunkIndices {
		slices := c.store.GetSlices(ctx, fileHandle, chunkIdx)
		for _, slice := range slices {
			if slice.State == cache.SliceStatePending {
				return true
			}
		}
	}

	return false
}

// GetFileSize returns the maximum extent of cached data.
func (c *MemoryCache) GetFileSize(fileHandle []byte) uint64 {
	if c.store.IsClosed() {
		return 0
	}

	ctx := context.Background()
	if !c.store.FileExists(ctx, fileHandle) {
		return 0
	}

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

	return uint64(maxChunk)*cache.ChunkSize + uint64(maxOffsetInChunk)
}

// ListFiles returns all file handles with cached data.
func (c *MemoryCache) ListFiles() [][]byte {
	if c.store.IsClosed() {
		return [][]byte{}
	}

	return c.store.ListFiles(context.Background())
}

// ============================================================================
// Lifecycle
// ============================================================================

// Close releases all cache resources.
func (c *MemoryCache) Close() error {
	return c.store.Close()
}
