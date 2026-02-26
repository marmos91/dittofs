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
// Unlike Evict, Remove deletes ALL data including dirty/pending blocks.
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

	size := entryMemorySize(entry)
	pendingSize := entryPendingSize(entry)
	delete(c.files, payloadID)

	if size > 0 {
		atomicSubtract(&c.totalSize, size)
	}
	if pendingSize > 0 {
		atomicSubtract(&c.pendingSize, pendingSize)
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
// Removes all blocks beyond the new size, and clears coverage for bytes
// beyond the truncation point in partial blocks.
//
// Note: Truncate only reduces size, never extends. Extending a file
// is done via Write.
//
// Idempotent: Returns nil if file doesn't exist or if newSize >= current size.
//
// Errors:
//   - ErrCacheClosed: cache has been closed
//   - context.Canceled/DeadlineExceeded: context was cancelled
func (c *Cache) Truncate(ctx context.Context, payloadID string, newSize uint64) error {
	if err := c.checkClosed(ctx); err != nil {
		return err
	}

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
			if size := chunkMemorySize(chk); size > 0 {
				atomicSubtract(&c.totalSize, size)
			}
			delete(entry.chunks, chunkIdx)
		} else if chunkIdx == newEndChunk {
			// Truncate blocks in the last chunk
			for blockIdx, blk := range chk.blocks {
				blockStart := blockIdx * BlockSize
				if blockStart >= newOffsetInEndChunk {
					// Remove entire block beyond truncation point
					if blk.data != nil {
						atomicSubtract(&c.totalSize, BlockSize)
					}
					delete(chk.blocks, blockIdx)
				} else {
					// Partial truncation within block - clear coverage beyond new size
					blockTruncPoint := newOffsetInEndChunk - blockStart
					if blk.dataSize > blockTruncPoint {
						blk.dataSize = blockTruncPoint
					}
					// Clear coverage bits beyond truncation point
					clearCoverageBeyond(blk.coverage, blockTruncPoint)
				}
			}
		}
	}

	return nil
}

// clearCoverageBeyond clears coverage bits from offset to end of block.
func clearCoverageBeyond(coverage []uint64, offset uint32) {
	if coverage == nil {
		return
	}

	startBit := offset / CoverageGranularity
	for bit := startBit; bit < uint32(len(coverage))*CoverageBitsPerWord; bit++ {
		wordIdx := bit / CoverageBitsPerWord
		bitInWord := bit % CoverageBitsPerWord
		if wordIdx < uint32(len(coverage)) {
			coverage[wordIdx] &^= 1 << bitInWord
		}
	}
}

// ============================================================================
// File State Queries
// ============================================================================

// HasDirtyData returns true if the file has any unflushed (pending) blocks.
//
// Use this to check if a file needs flushing before close, or to prevent
// eviction of files with dirty data. A file with dirty data should not
// be removed until its blocks are flushed to the block store.
//
// Thread-safe. Returns false if cache is closed, context is cancelled, or file doesn't exist.
func (c *Cache) HasDirtyData(ctx context.Context, payloadID string) bool {
	if c.checkClosed(ctx) != nil {
		return false
	}

	entry := c.getFileEntry(payloadID)
	entry.mu.RLock()
	defer entry.mu.RUnlock()

	for _, chunk := range entry.chunks {
		for _, blk := range chunk.blocks {
			if blk.state == BlockStatePending {
				return true
			}
		}
	}

	return false
}

// GetFileSize returns the maximum byte offset covered by cached blocks and
// whether the file exists in cache at all.
//
// This represents the size of the file as known to the cache. Note that
// this may differ from the actual file size if not all data is cached.
//
// Returns (0, false) if the file doesn't exist in cache, cache is closed,
// or context is cancelled. Returns (0, true) for a zero-length cached file
// (e.g., after Truncate to 0).
func (c *Cache) GetFileSize(ctx context.Context, payloadID string) (uint64, bool) {
	if c.checkClosed(ctx) != nil {
		return 0, false
	}

	c.globalMu.RLock()
	entry, exists := c.files[payloadID]
	c.globalMu.RUnlock()

	if !exists {
		return 0, false
	}

	entry.mu.RLock()
	defer entry.mu.RUnlock()

	return getFileSizeUnlocked(entry), true
}

// getFileSizeUnlocked returns the maximum byte offset covered by cached blocks.
// Caller must hold entry.mu (read or write lock).
func getFileSizeUnlocked(entry *fileEntry) uint64 {
	var maxOffset uint64

	for chunkIdx, chunk := range entry.chunks {
		chunkBase := uint64(chunkIdx) * ChunkSize
		for blockIdx, blk := range chunk.blocks {
			if blk.data == nil {
				continue
			}
			blockBase := chunkBase + uint64(blockIdx)*BlockSize
			if end := blockBase + uint64(blk.dataSize); end > maxOffset {
				maxOffset = end
			}
		}
	}

	return maxOffset
}

// entryMemorySize returns total memory allocated by a file entry's blocks.
// Returns BlockSize per block buffer (actual memory allocation), not bytes written.
func entryMemorySize(entry *fileEntry) uint64 {
	var size uint64
	for _, chunk := range entry.chunks {
		size += chunkMemorySize(chunk)
	}
	return size
}

// chunkMemorySize returns total memory allocated by a chunk's blocks.
// Returns BlockSize per block buffer (actual memory allocation), not bytes written.
func chunkMemorySize(chunk *chunkEntry) uint64 {
	var count uint64
	for _, blk := range chunk.blocks {
		if blk.data != nil {
			count++
		}
	}
	return count * BlockSize
}

// entryPendingSize returns total memory allocated by pending blocks.
// Returns BlockSize per pending block buffer.
func entryPendingSize(entry *fileEntry) uint64 {
	var count uint64
	for _, chunk := range entry.chunks {
		for _, blk := range chunk.blocks {
			if blk.data != nil && blk.state != BlockStateUploaded {
				count++
			}
		}
	}
	return count * BlockSize
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
// covered by any block. This is used during crash recovery to reconcile
// metadata with actual recovered data.
//
// Returns empty map if cache is closed.
func (c *Cache) ListFilesWithSizes() map[string]uint64 {
	c.globalMu.RLock()
	defer c.globalMu.RUnlock()

	if c.closed {
		return map[string]uint64{}
	}

	result := make(map[string]uint64, len(c.files))
	for key, entry := range c.files {
		entry.mu.RLock()
		result[key] = getFileSizeUnlocked(entry)
		entry.mu.RUnlock()
	}

	return result
}

// GetTotalSize returns the total memory allocated by the cache.
//
// This tracks BlockSize (4MB) per block buffer, regardless of how many
// bytes are written to each buffer. Use this for OOM prevention monitoring.
// Use Stats() for a breakdown of actual data written.
func (c *Cache) GetTotalSize() uint64 {
	return c.totalSize.Load()
}

// Stats returns current cache statistics for observability.
//
// Returns:
//   - TotalSize: Memory allocated (BlockSize per block buffer)
//   - MaxSize: Configured maximum (0 = unlimited)
//   - FileCount: Number of files with cached data
//   - DirtyBytes: Actual data bytes in pending/uploading state (protected from eviction)
//   - UploadedBytes: Actual data bytes in uploaded state (can be evicted)
//   - BlockCount: Total number of block buffers
//
// Note: TotalSize tracks memory allocation, while DirtyBytes+UploadedBytes
// tracks actual data written. They may differ since each block buffer
// allocates BlockSize (4MB) regardless of content.
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
			for _, blk := range chunk.blocks {
				if blk.data == nil {
					continue
				}
				stats.BlockCount++
				size := uint64(blk.dataSize)
				if blk.state == BlockStateUploaded {
					stats.UploadedBytes += size
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
