package cache

import (
	"cmp"
	"context"
	"slices"
	"time"
)

// ============================================================================
// Cache Management (LRU Eviction)
// ============================================================================
//
// The cache uses LRU (Least Recently Used) eviction to stay within maxSize.
// Only uploaded blocks can be evicted - dirty data (pending/uploading) is protected.
// This ensures data durability: unflushed writes are never lost due to cache pressure.
//
// Eviction is triggered automatically on Write when the cache would exceed
// maxSize, or manually via EvictLRU/Evict/EvictAll.

// evictLRUUntilFits evicts uploaded blocks to make room for new data.
//
// Called automatically by Write when cache would exceed maxSize.
// Evicts from least recently used files first.
func (c *Cache) evictLRUUntilFits(ctx context.Context, neededBytes uint64) {
	if c.maxSize == 0 {
		return
	}
	c.evictLRUToTarget(ctx, c.maxSize-neededBytes)
}

// evictLRUToTarget evicts uploaded blocks from LRU files until size <= target.
// Checks context cancellation between file evictions.
func (c *Cache) evictLRUToTarget(ctx context.Context, targetSize uint64) {
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
	slices.SortFunc(files, func(a, b fileAccess) int {
		return cmp.Compare(a.lastAccess.UnixNano(), b.lastAccess.UnixNano())
	})

	// Evict until target reached, respecting context cancellation
	for _, f := range files {
		// Check context between file evictions
		if ctx.Err() != nil {
			return
		}
		if c.totalSize.Load() <= targetSize {
			break
		}
		c.evictUploadedFromEntry(f.payloadID)
	}
}

// evictUploadedFromEntry removes uploaded blocks from a file entry.
// Returns bytes evicted. Caller must NOT hold any locks.
func (c *Cache) evictUploadedFromEntry(payloadID string) uint64 {
	c.globalMu.RLock()
	entry, exists := c.files[payloadID]
	c.globalMu.RUnlock()

	if !exists {
		return 0
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()

	return c.evictUploadedBlocks(entry)
}

// evictUploadedBlocks removes uploaded blocks from an entry.
// Caller must hold entry.mu write lock.
func (c *Cache) evictUploadedBlocks(entry *fileEntry) uint64 {
	var evicted uint64

	for chunkIdx, chunk := range entry.chunks {
		for blockIdx, blk := range chunk.blocks {
			if blk.state != BlockStateUploaded {
				continue
			}

			if blk.data == nil {
				continue
			}

			// Evict by clearing the data buffer.
			// Subtract BlockSize since that's what we track for memory allocation.
			evicted += BlockSize
			c.totalSize.Add(^uint64(BlockSize - 1)) // Atomic subtract

			blk.data = nil
			blk.coverage = nil
			blk.dataSize = 0

			// Remove the block entry entirely
			delete(chunk.blocks, blockIdx)
		}

		// Clean up empty chunks
		if len(chunk.blocks) == 0 {
			delete(entry.chunks, chunkIdx)
		}
	}

	return evicted
}

// EvictLRU evicts uploaded blocks from least recently used files to free space.
//
// Use this for explicit cache management, e.g., before a large operation or
// during low-activity periods. Automatic eviction via Write is usually sufficient.
//
// Only uploaded blocks are evicted - dirty data is protected.
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

	c.evictLRUToTarget(ctx, targetSize)

	if endSize := c.totalSize.Load(); startSize > endSize {
		return startSize - endSize, nil
	}
	return 0, nil
}

// Evict removes all uploaded blocks for a specific file.
//
// Use this when a file is closed or deleted to free its cache space immediately.
// Only uploaded blocks are removed - dirty data is protected.
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

	return c.evictUploadedBlocks(entry), nil
}

// EvictAll removes all uploaded blocks from all files in the cache.
//
// Use this for aggressive cache clearing, e.g., during shutdown preparation
// or when switching storage backends. Only uploaded blocks are removed - dirty
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
