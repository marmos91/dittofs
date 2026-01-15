package cache

import (
	"context"
	"sort"
	"time"
)

// ============================================================================
// Cache Management (LRU Eviction)
// ============================================================================
//
// The cache uses LRU (Least Recently Used) eviction to stay within maxSize.
// Only flushed slices can be evicted - dirty data (pending/uploading) is protected.
// This ensures data durability: unflushed writes are never lost due to cache pressure.
//
// Eviction is triggered automatically on WriteSlice when the cache would exceed
// maxSize, or manually via EvictLRU/Evict/EvictAll.

// evictLRUUntilFits evicts flushed slices to make room for new data.
//
// Called automatically by WriteSlice when cache would exceed maxSize.
// Evicts from least recently used files first.
func (c *Cache) evictLRUUntilFits(neededBytes uint64) {
	if c.maxSize == 0 {
		return
	}
	c.evictLRUToTarget(c.maxSize - neededBytes)
}

// evictLRUToTarget evicts flushed slices from LRU files until size <= target.
func (c *Cache) evictLRUToTarget(targetSize uint64) {
	type fileAccess struct {
		payloadID  string
		lastAccess time.Time
	}

	// Snapshot file access times under lock
	c.globalMu.RLock()
	files := make([]fileAccess, 0, len(c.files))
	for payloadID, entry := range c.files {
		entry.mu.RLock()
		files = append(files, fileAccess{payloadID, entry.lastAccess})
		entry.mu.RUnlock()
	}
	c.globalMu.RUnlock()

	// Sort oldest first
	sort.Slice(files, func(i, j int) bool {
		return files[i].lastAccess.Before(files[j].lastAccess)
	})

	// Evict until target reached
	for _, f := range files {
		if c.totalSize.Load() <= targetSize {
			break
		}
		c.evictFlushedFromEntry(f.payloadID)
	}
}

// evictFlushedFromEntry removes flushed slices from a file entry.
// Returns bytes evicted. Caller must NOT hold any locks.
func (c *Cache) evictFlushedFromEntry(payloadID string) uint64 {
	c.globalMu.RLock()
	entry, exists := c.files[payloadID]
	c.globalMu.RUnlock()

	if !exists {
		return 0
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()

	return c.evictFlushedSlices(entry)
}

// evictFlushedSlices removes flushed slices from an entry.
// Caller must hold entry.mu write lock.
func (c *Cache) evictFlushedSlices(entry *fileEntry) uint64 {
	var evicted uint64

	for chunkIdx, chunk := range entry.chunks {
		// partitionByState returns (matching, other) - flushed slices are to evict
		toEvict, toKeep := partitionByState(chunk.slices, SliceStateFlushed)
		if len(toEvict) == 0 {
			continue
		}

		for _, s := range toEvict {
			size := uint64(len(s.Data))
			evicted += size
			c.totalSize.Add(^(size - 1)) // Atomic subtract
		}

		if len(toKeep) == 0 {
			delete(entry.chunks, chunkIdx)
		} else {
			chunk.slices = toKeep
		}
	}

	return evicted
}

// EvictLRU evicts flushed slices from least recently used files to free space.
//
// Use this for explicit cache management, e.g., before a large operation or
// during low-activity periods. Automatic eviction via WriteSlice is usually sufficient.
//
// Only flushed slices are evicted - dirty data is protected.
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
	targetSize := uint64(0)
	if startSize > targetFreeBytes {
		targetSize = startSize - targetFreeBytes
	}

	c.evictLRUToTarget(targetSize)

	if endSize := c.totalSize.Load(); startSize > endSize {
		return startSize - endSize, nil
	}
	return 0, nil
}

// Evict removes all flushed slices for a specific file.
//
// Use this when a file is closed or deleted to free its cache space immediately.
// Only flushed slices are removed - dirty data is protected.
func (c *Cache) Evict(ctx context.Context, payloadID string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return 0, ErrCacheClosed
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(payloadID)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	return c.evictFlushedSlices(entry), nil
}

// EvictAll removes all flushed slices from all files in the cache.
//
// Use this for aggressive cache clearing, e.g., during shutdown preparation
// or when switching storage backends. Only flushed slices are removed - dirty
// data is protected.
//
// Returns:
//   - evicted: Total bytes evicted across all files
//   - error: Context errors or ErrCacheClosed
func (c *Cache) EvictAll(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return 0, ErrCacheClosed
	}
	payloadIDs := make([]string, 0, len(c.files))
	for k := range c.files {
		payloadIDs = append(payloadIDs, k)
	}
	c.globalMu.RUnlock()

	var total uint64
	for _, payloadID := range payloadIDs {
		evicted, err := c.Evict(ctx, payloadID)
		if err != nil {
			return total, err
		}
		total += evicted
	}

	return total, nil
}
