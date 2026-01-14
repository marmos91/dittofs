package cache

import (
	"context"

	"github.com/marmos91/dittofs/pkg/payload/chunk"
)

// ============================================================================
// File Operations
// ============================================================================

// Remove completely removes all cached data for a file.
func (c *Cache) Remove(ctx context.Context, fileHandle string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.globalMu.Lock()
	defer c.globalMu.Unlock()

	if c.closed {
		return ErrCacheClosed
	}

	key := fileHandle
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

	// Persist removal to WAL if enabled
	if c.persister.IsEnabled() {
		if err := c.persister.AppendRemove(fileHandle); err != nil {
			return err
		}
	}

	return nil
}

// Truncate changes the size of cached data for a file.
func (c *Cache) Truncate(ctx context.Context, fileHandle string, newSize uint64) error {
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

	newEndChunk := chunk.IndexForOffset(newSize)
	newOffsetInEndChunk := chunk.OffsetInChunk(newSize)

	for chunkIdx, chk := range entry.chunks {
		if chunkIdx > newEndChunk {
			// Calculate size to subtract
			var size uint64
			for _, slice := range chk.slices {
				size += uint64(len(slice.Data))
			}
			if size > 0 {
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

// HasDirtyData returns true if the file has pending slices.
func (c *Cache) HasDirtyData(fileHandle string) bool {
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
func (c *Cache) GetFileSize(fileHandle string) uint64 {
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

	// Close persister if enabled
	if c.persister != nil && c.persister.IsEnabled() {
		if err := c.persister.Close(); err != nil {
			return err
		}
	}

	return nil
}

// Sync forces pending writes to durable storage.
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
