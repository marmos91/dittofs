package cache

import (
	"context"

	"github.com/marmos91/dittofs/pkg/payload/chunk"
)

// ============================================================================
// File Operations
// ============================================================================

// Remove completely removes all cached data for a file.
//
// This should be called when a file is deleted from the filesystem.
// Unlike Evict, Remove deletes ALL data including dirty/pending slices.
// The removal is also persisted to WAL if enabled.
//
// Idempotent: Returns nil if file doesn't exist.
//
// Errors:
//   - ErrCacheClosed: cache has been closed
//   - context.Canceled/DeadlineExceeded: context was cancelled
func (c *Cache) Remove(ctx context.Context, payloadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.globalMu.Lock()
	defer c.globalMu.Unlock()

	if c.closed {
		return ErrCacheClosed
	}

	entry, exists := c.files[payloadID]
	if !exists {
		return nil // Idempotent
	}

	size := entryDataSize(entry)
	delete(c.files, payloadID)

	if size > 0 {
		c.totalSize.Add(^(size - 1)) // Subtract
	}

	// Persist removal to WAL if enabled
	if c.persister != nil {
		if err := c.persister.AppendRemove(payloadID); err != nil {
			return err
		}
	}

	return nil
}

// Truncate reduces the size of cached data for a file.
//
// This should be called when a file is truncated in the filesystem.
// Removes all slices beyond the new size, and trims slices that span
// the truncation point.
//
// Note: Truncate only reduces size, never extends. Extending a file
// is done via WriteSlice.
//
// Idempotent: Returns nil if file doesn't exist or if newSize >= current size.
//
// Errors:
//   - ErrCacheClosed: cache has been closed
//   - context.Canceled/DeadlineExceeded: context was cancelled
func (c *Cache) Truncate(ctx context.Context, payloadID string, newSize uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return ErrCacheClosed
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(payloadID)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	if len(entry.chunks) == 0 {
		return nil
	}

	currentSize := getFileSizeUnlocked(entry)
	if newSize >= currentSize {
		return nil
	}

	newEndChunk := chunk.IndexForOffset(newSize)
	newOffsetInEndChunk := chunk.OffsetInChunk(newSize)

	for chunkIdx, chk := range entry.chunks {
		if chunkIdx > newEndChunk {
			// Remove entire chunk beyond truncation point
			if size := chunkDataSize(chk); size > 0 {
				c.totalSize.Add(^(size - 1)) // Subtract
			}
			delete(entry.chunks, chunkIdx)
		} else if chunkIdx == newEndChunk {
			var newSlices []Slice
			var removedSize uint64

			for _, slice := range chk.slices {
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
				chk.slices = newSlices
			}
		}
	}

	return nil
}

// ============================================================================
// File State Queries
// ============================================================================

// HasDirtyData returns true if the file has any unflushed (pending) slices.
//
// Use this to check if a file needs flushing before close, or to prevent
// eviction of files with dirty data. A file with dirty data should not
// be removed until its slices are flushed to the block store.
//
// Thread-safe. Returns false if cache is closed or file doesn't exist.
func (c *Cache) HasDirtyData(payloadID string) bool {
	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return false
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(payloadID)
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

// GetFileSize returns the maximum byte offset covered by cached slices.
//
// This represents the size of the file as known to the cache. Note that
// this may differ from the actual file size if not all data is cached.
//
// Returns 0 if the file doesn't exist in cache or cache is closed.
func (c *Cache) GetFileSize(payloadID string) uint64 {
	c.globalMu.RLock()
	if c.closed {
		c.globalMu.RUnlock()
		return 0
	}
	c.globalMu.RUnlock()

	entry := c.getFileEntry(payloadID)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	return getFileSizeUnlocked(entry)
}

// getFileSizeUnlocked returns the maximum byte offset covered by cached slices.
// Caller must hold entry.mu (read or write lock).
func getFileSizeUnlocked(entry *fileEntry) uint64 {
	var maxOffset uint64

	for chunkIdx, chunk := range entry.chunks {
		chunkBase := uint64(chunkIdx) * ChunkSize
		for _, slice := range chunk.slices {
			if end := chunkBase + uint64(slice.Offset+slice.Length); end > maxOffset {
				maxOffset = end
			}
		}
	}

	return maxOffset
}

// entryDataSize returns total bytes stored in a file entry's slices.
func entryDataSize(entry *fileEntry) uint64 {
	var size uint64
	for _, chunk := range entry.chunks {
		size += chunkDataSize(chunk)
	}
	return size
}

// chunkDataSize returns total bytes stored in a chunk's slices.
func chunkDataSize(chunk *chunkEntry) uint64 {
	var size uint64
	for _, slice := range chunk.slices {
		size += uint64(len(slice.Data))
	}
	return size
}

// ListFiles returns all file handles that have cached data.
//
// Use this for debugging, cache inspection, or iterating over cached files.
// The returned order is not guaranteed.
//
// Returns empty slice if cache is closed.
func (c *Cache) ListFiles() []string {
	c.globalMu.RLock()
	defer c.globalMu.RUnlock()

	if c.closed {
		return []string{}
	}

	result := make([]string, 0, len(c.files))
	for key := range c.files {
		result = append(result, key)
	}

	return result
}

// ListFilesWithSizes returns all cached files with their calculated sizes.
//
// For each file in cache, the size is calculated as the maximum byte offset
// covered by any slice. This is used during crash recovery to reconcile
// metadata with actual recovered data.
//
// Returns nil if cache is closed.
func (c *Cache) ListFilesWithSizes() map[string]uint64 {
	c.globalMu.RLock()
	defer c.globalMu.RUnlock()

	if c.closed {
		return nil
	}

	result := make(map[string]uint64, len(c.files))
	for key, entry := range c.files {
		entry.mu.RLock()
		result[key] = getFileSizeUnlocked(entry)
		entry.mu.RUnlock()
	}

	return result
}

// GetTotalSize returns the total bytes currently stored in the cache.
//
// This includes both dirty (pending) and flushed data. Use Stats() for
// a breakdown by state.
func (c *Cache) GetTotalSize() uint64 {
	return c.totalSize.Load()
}

// Stats returns current cache statistics for observability.
//
// Returns:
//   - TotalSize: Current total bytes in cache
//   - MaxSize: Configured maximum (0 = unlimited)
//   - FileCount: Number of files with cached data
//   - DirtyBytes: Bytes in pending/uploading state (protected from eviction)
//   - FlushedBytes: Bytes in flushed state (can be evicted)
//   - SliceCount: Total number of slices
//
// Returns zero Stats if cache is closed.
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

// Close releases all cache resources and closes the WAL persister.
//
// After Close, all operations return ErrCacheClosed. Any unflushed data
// will be lost unless it was persisted to WAL and the cache is reopened.
//
// Idempotent: Safe to call multiple times.
func (c *Cache) Close() error {
	c.globalMu.Lock()
	defer c.globalMu.Unlock()

	if c.closed {
		return nil
	}

	c.closed = true
	c.files = nil
	c.totalSize.Store(0)

	// Close persister if enabled
	if c.persister != nil {
		if err := c.persister.Close(); err != nil {
			return err
		}
	}

	return nil
}

// Sync forces WAL data to durable storage (fsync).
//
// Call this when durability is required, e.g., on NFS COMMIT or before
// reporting success to the client. Without Sync, WAL data may be buffered
// by the OS page cache.
//
// No-op if WAL persistence is not enabled.
//
// Errors:
//   - ErrCacheClosed: cache has been closed
//   - I/O errors from the underlying persister
func (c *Cache) Sync() error {
	c.globalMu.RLock()
	defer c.globalMu.RUnlock()

	if c.closed {
		return ErrCacheClosed
	}

	if c.persister == nil {
		return nil
	}

	return c.persister.Sync()
}
