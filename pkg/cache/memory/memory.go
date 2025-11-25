// Package memory implements an in-memory cache for content stores.
//
// This package provides a fast in-memory buffer for caching file content
// before it's flushed to persistent storage backends.
package memory

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// Default buffer capacity per file (5MB - matches S3 multipart threshold)
const defaultBufferCapacity = 5 * 1024 * 1024

// ============================================================================
// MemoryCache - In-memory implementation
// ============================================================================

// buffer represents a single file's write buffer.
type buffer struct {
	data      []byte
	lastWrite time.Time
	mu        sync.Mutex
}

// MemoryCache manages in-memory buffers for multiple files.
//
// This is an implementation of the Cache interface that stores all data
// in memory. It's very fast but limited by available RAM.
//
// Characteristics:
//   - Very fast (no I/O overhead)
//   - Limited by available RAM
//   - Best for small to medium files (< 100MB)
//   - Simple implementation
//   - Optional size limit with eviction support
//
// Memory Usage:
// Each file gets its own buffer. Total memory = sum of all buffer sizes.
// Buffers are released when Remove() is called or on Close().
//
// Thread Safety:
// Safe for concurrent use. Operations on different content IDs are fully
// parallel. Operations on the same content ID are serialized.
type MemoryCache struct {
	buffers map[string]*buffer
	mu      sync.RWMutex
	closed  bool
	maxSize int64              // Maximum total cache size (0 = unlimited)
	metrics cache.CacheMetrics // Optional metrics collector
}

// NewMemoryCache creates a new in-memory cache.
//
// Parameters:
//   - maxSize: Maximum total cache size in bytes (0 = unlimited)
//   - metrics: Optional metrics collector (can be nil for no metrics)
//
// Returns:
//   - cache.Cache: New cache instance
func NewMemoryCache(maxSize int64, metrics cache.CacheMetrics) cache.Cache {
	return &MemoryCache{
		buffers: make(map[string]*buffer),
		closed:  false,
		maxSize: maxSize,
		metrics: metrics,
	}
}

// getOrCreateBuffer retrieves an existing buffer or creates a new one.
//
// This helper reduces code duplication and simplifies locking.
func (c *MemoryCache) getOrCreateBuffer(id metadata.ContentID) (*buffer, error) {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return nil, fmt.Errorf("cache is closed")
	}

	idStr := string(id)
	buf, exists := c.buffers[idStr]
	c.mu.RUnlock()

	if !exists {
		// Need to create new buffer
		c.mu.Lock()
		// Double-check after acquiring write lock
		buf, exists = c.buffers[idStr]
		if !exists {
			buf = &buffer{
				data:      make([]byte, 0, defaultBufferCapacity),
				lastWrite: time.Now(),
			}
			c.buffers[idStr] = buf
		}
		c.mu.Unlock()
	}

	return buf, nil
}

// getBuffer retrieves an existing buffer (does not create).
func (c *MemoryCache) getBuffer(id metadata.ContentID) (*buffer, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.closed {
		return nil, false
	}

	idStr := string(id)
	buf, exists := c.buffers[idStr]
	return buf, exists
}

// Write replaces the entire content for a content ID.
func (c *MemoryCache) Write(ctx context.Context, id metadata.ContentID, data []byte) error {
	// Check context before starting
	if err := ctx.Err(); err != nil {
		return err
	}

	start := time.Now()
	defer func() {
		if c.metrics != nil {
			c.metrics.ObserveWrite(int64(len(data)), time.Since(start))
		}
	}()

	buf, err := c.getOrCreateBuffer(id)
	if err != nil {
		return err
	}

	buf.mu.Lock()
	// Replace entire buffer
	buf.data = make([]byte, len(data))
	copy(buf.data, data)
	buf.lastWrite = time.Now()
	bufSize := int64(len(buf.data))
	buf.mu.Unlock()

	// Record cache size after write (must be done after releasing buf lock)
	if c.metrics != nil {
		c.metrics.RecordCacheSize(string(id), bufSize)

		// Need to acquire lock to update total cache size and buffer count
		c.mu.RLock()
		c.metrics.RecordBufferCount(len(c.buffers))
		c.updateTotalCacheSize()
		c.mu.RUnlock()
	}

	return nil
}

// WriteAt writes data at the specified offset for a content ID.
func (c *MemoryCache) WriteAt(ctx context.Context, id metadata.ContentID, data []byte, offset int64) error {
	// Check context before starting
	if err := ctx.Err(); err != nil {
		return err
	}

	start := time.Now()
	defer func() {
		if c.metrics != nil {
			c.metrics.ObserveWrite(int64(len(data)), time.Since(start))
		}
	}()

	if offset < 0 {
		return fmt.Errorf("negative offset: %d", offset)
	}

	buf, err := c.getOrCreateBuffer(id)
	if err != nil {
		return err
	}

	buf.mu.Lock()
	writeEnd := offset + int64(len(data))
	currentSize := int64(len(buf.data))

	// Extend buffer if needed
	if writeEnd > currentSize {
		newSize := writeEnd
		if newSize > int64(cap(buf.data)) {
			// Need to reallocate - use exponential growth strategy
			// This prevents O(N²) behavior when writing large files sequentially
			newCap := int64(cap(buf.data))
			if newCap == 0 {
				newCap = defaultBufferCapacity
			}

			// Double capacity until it's large enough, or add 10MB chunks for very large files
			for newCap < newSize {
				if newCap < 100*1024*1024 { // < 100MB: double it
					newCap *= 2
				} else { // >= 100MB: grow by 10MB chunks
					newCap += 10 * 1024 * 1024
				}
			}

			newBuf := make([]byte, newSize, newCap)
			copy(newBuf, buf.data)
			buf.data = newBuf
		} else {
			// Just extend length
			buf.data = buf.data[:newSize]
		}

		// Fill gap with zeros if offset > current size (sparse file)
		if offset > currentSize {
			for i := currentSize; i < offset; i++ {
				buf.data[i] = 0
			}
		}
	}

	// Copy data at offset
	copy(buf.data[offset:], data)
	buf.lastWrite = time.Now()
	bufSize := int64(len(buf.data))
	buf.mu.Unlock()

	// Record cache size after write (must be done after releasing buf lock)
	if c.metrics != nil {
		c.metrics.RecordCacheSize(string(id), bufSize)

		// Need to acquire lock to update total cache size and buffer count
		c.mu.RLock()
		c.metrics.RecordBufferCount(len(c.buffers))
		c.updateTotalCacheSize()
		c.mu.RUnlock()
	}

	return nil
}

// Read returns all cached data for a content ID.
func (c *MemoryCache) Read(ctx context.Context, id metadata.ContentID) ([]byte, error) {
	// Check context before starting
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return []byte{}, nil
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	// Return a copy to avoid data races
	result := make([]byte, len(cacheBuf.data))
	copy(result, cacheBuf.data)

	return result, nil
}

// ReadAt reads data from the cache at the specified offset.
func (c *MemoryCache) ReadAt(ctx context.Context, id metadata.ContentID, buf []byte, offset int64) (int, error) {
	// Check context before starting
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	start := time.Now()
	defer func() {
		if c.metrics != nil {
			c.metrics.ObserveRead(int64(len(buf)), time.Since(start))
		}
	}()

	if offset < 0 {
		return 0, fmt.Errorf("negative offset: %d", offset)
	}

	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return 0, io.EOF
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	if offset >= int64(len(cacheBuf.data)) {
		return 0, io.EOF
	}

	n := copy(buf, cacheBuf.data[offset:])
	if n < len(buf) {
		return n, io.EOF
	}

	return n, nil
}

// Size returns the size of cached data for a content ID.
func (c *MemoryCache) Size(id metadata.ContentID) int64 {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return 0
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	return int64(len(cacheBuf.data))
}

// LastWrite returns the timestamp of the last write for a content ID.
func (c *MemoryCache) LastWrite(id metadata.ContentID) time.Time {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return time.Time{}
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	return cacheBuf.lastWrite
}

// Exists checks if cached data exists for a content ID.
func (c *MemoryCache) Exists(id metadata.ContentID) bool {
	_, exists := c.getBuffer(id)
	return exists
}

// List returns all content IDs with cached data.
func (c *MemoryCache) List() []metadata.ContentID {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.closed {
		return []metadata.ContentID{}
	}

	result := make([]metadata.ContentID, 0, len(c.buffers))
	for idStr := range c.buffers {
		result = append(result, metadata.ContentID(idStr))
	}

	return result
}

// updateTotalCacheSize calculates and records the total cache size across all buffers.
// Caller must hold c.mu (RLock or Lock).
func (c *MemoryCache) updateTotalCacheSize() {
	// Lock must be held by caller
	var totalSize int64
	for _, buf := range c.buffers {
		buf.mu.Lock()
		totalSize += int64(len(buf.data))
		buf.mu.Unlock()
	}

	if c.metrics != nil {
		c.metrics.RecordTotalCacheSize(totalSize)
	}
}

// Remove clears the cached data for a specific content ID.
func (c *MemoryCache) Remove(id metadata.ContentID) error {
	c.mu.Lock()
	defer func() {
		// Record buffer count and total size after removal
		if c.metrics != nil {
			c.metrics.RecordBufferCount(len(c.buffers))
			c.updateTotalCacheSize()
		}
		c.mu.Unlock()
	}()

	if c.closed {
		return fmt.Errorf("cache is closed")
	}

	idStr := string(id)
	buf, exists := c.buffers[idStr]
	if !exists {
		return nil // Already removed (idempotent)
	}

	// Clear buffer data
	buf.mu.Lock()
	buf.data = nil
	buf.mu.Unlock()

	// Remove from map
	delete(c.buffers, idStr)

	// Record cache removal
	if c.metrics != nil {
		c.metrics.RecordCacheReset(idStr)
	}

	return nil
}

// RemoveAll clears all cached data for all content IDs.
func (c *MemoryCache) RemoveAll() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return fmt.Errorf("cache is closed")
	}

	// Clear all buffers
	for _, buf := range c.buffers {
		buf.mu.Lock()
		buf.data = nil
		buf.mu.Unlock()
	}

	// Clear the map
	c.buffers = make(map[string]*buffer)

	// Record metrics after clearing
	if c.metrics != nil {
		c.metrics.RecordBufferCount(0)
		c.metrics.RecordTotalCacheSize(0)
	}

	return nil
}

// TotalSize returns the total size of all cached data across all files.
func (c *MemoryCache) TotalSize() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.closed {
		return 0
	}

	var total int64
	for _, buf := range c.buffers {
		buf.mu.Lock()
		total += int64(len(buf.data))
		buf.mu.Unlock()
	}

	return total
}

// MaxSize returns the maximum cache size configured for this cache.
func (c *MemoryCache) MaxSize() int64 {
	return c.maxSize
}

// Close releases all resources and clears all cached data.
func (c *MemoryCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil // Already closed (idempotent)
	}

	// Clear all buffers
	for _, buf := range c.buffers {
		buf.mu.Lock()
		buf.data = nil
		buf.mu.Unlock()
	}

	c.buffers = nil
	c.closed = true

	return nil
}

// ============================================================================
// Helper functions
// ============================================================================

// BufferInfo contains diagnostic information about a buffer.
type BufferInfo struct {
	Size      int64
	LastWrite time.Time
}

// GetInfo returns diagnostic information about all buffers.
//
// This is useful for debugging and monitoring.
//
// Parameters:
//   - c: Cache to inspect
//
// Returns:
//   - map: Content ID -> buffer info
func GetInfo(c cache.Cache) map[metadata.ContentID]BufferInfo {
	result := make(map[metadata.ContentID]BufferInfo)

	for _, id := range c.List() {
		result[id] = BufferInfo{
			Size:      c.Size(id),
			LastWrite: c.LastWrite(id),
		}
	}

	return result
}
