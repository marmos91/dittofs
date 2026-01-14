package cache

import (
	"context"
	"sort"
	"time"
)

// ============================================================================
// Cache Management (LRU Eviction)
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
func (c *Cache) Evict(ctx context.Context, fileHandle string) (uint64, error) {
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
	handles := make([]string, 0, len(c.files))
	for k := range c.files {
		handles = append(handles, k)
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
