// Package cache implements write caching for content stores.
//
// This package provides abstractions for buffering writes before flushing to
// storage backends, eliminating expensive read-modify-write cycles.
package cache

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// Default buffer capacity per file (5MB - matches S3 multipart threshold)
const defaultBufferCapacity = 5 * 1024 * 1024

// WriteCache manages write buffers for multiple content IDs concurrently.
//
// Design:
// The cache maintains a map of ContentID -> buffer, allowing parallel writes
// to different files. Each buffer is independent and can be flushed separately.
//
// Use Cases:
//   - NFS server handling multiple concurrent writes
//   - Buffering writes to S3, flushing at threshold or file close
//   - Accumulating sequential writes to avoid read-modify-write
//
// Thread Safety:
// Implementations must be safe for concurrent operations across different
// content IDs. Operations on the same content ID are serialized internally.
type WriteCache interface {
	// WriteAt writes data at the specified offset for a content ID.
	//
	// If no buffer exists for the ID, one is created automatically.
	// If offset > current size, the gap is filled with zeros (sparse file behavior).
	// If offset < current size, existing data is overwritten.
	//
	// Parameters:
	//   - id: Content identifier
	//   - data: Data to write
	//   - offset: Byte offset where writing begins
	//
	// Returns:
	//   - error: Returns error if write fails (e.g., out of memory, cache closed)
	WriteAt(id metadata.ContentID, data []byte, offset int64) error

	// ReadAt reads data from the cache at the specified offset.
	//
	// Implements io.ReaderAt pattern for reading partial cache data.
	// Useful for multipart uploads and incremental flushing.
	//
	// Parameters:
	//   - id: Content identifier
	//   - buf: Buffer to read into
	//   - offset: Byte offset to start reading from
	//
	// Returns:
	//   - int: Number of bytes read
	//   - error: io.EOF if offset >= size, error on failure
	ReadAt(id metadata.ContentID, buf []byte, offset int64) (int, error)

	// ReadAll returns all cached data for a specific content ID.
	//
	// This is used when flushing the cache to storage. The buffer is NOT
	// cleared - call Reset() after successful flush.
	//
	// Parameters:
	//   - id: Content identifier
	//
	// Returns:
	//   - []byte: All cached data (empty if no buffer exists)
	//   - error: Returns error if read fails (e.g., cache closed)
	ReadAll(id metadata.ContentID) ([]byte, error)

	// Size returns the size of cached data for a specific content ID.
	//
	// Parameters:
	//   - id: Content identifier
	//
	// Returns:
	//   - int64: Size in bytes (0 if no buffer exists or cache closed)
	Size(id metadata.ContentID) int64

	// Reset clears the buffer for a specific content ID.
	//
	// After reset, the buffer is deleted from the cache. Subsequent writes
	// will create a new buffer.
	//
	// Parameters:
	//   - id: Content identifier
	//
	// Returns:
	//   - error: Returns error if reset fails
	Reset(id metadata.ContentID) error

	// ResetAll clears all buffers for all content IDs.
	//
	// This is useful for:
	//   - Server shutdown (after flushing all buffers)
	//   - Testing (clean slate)
	//   - Emergency cleanup
	//
	// Returns:
	//   - error: Returns error if reset fails
	ResetAll() error

	// LastWrite returns the timestamp of the last write for a content ID.
	//
	// This is used for timeout-based auto-flush. Returns zero time if no
	// buffer exists.
	//
	// Parameters:
	//   - id: Content identifier
	//
	// Returns:
	//   - time.Time: Last write timestamp (zero if no buffer exists or cache closed)
	LastWrite(id metadata.ContentID) time.Time

	// List returns all content IDs with active buffers.
	//
	// This is used for auto-flush workers to iterate over all cached files.
	//
	// Returns:
	//   - []metadata.ContentID: List of content IDs with cached data
	List() []metadata.ContentID

	// Close releases all resources and clears all buffers.
	//
	// After Close, the cache cannot be used. All subsequent operations will fail.
	//
	// Returns:
	//   - error: Returns error if cleanup fails
	Close() error
}

// ============================================================================
// MemoryWriteCache - In-memory implementation
// ============================================================================

// buffer represents a single file's write buffer.
type buffer struct {
	data      []byte
	lastWrite time.Time
	mu        sync.Mutex
}

// MemoryWriteCache manages in-memory write buffers for multiple files.
//
// Characteristics:
//   - Very fast (no I/O overhead)
//   - Limited by available RAM
//   - Best for small to medium files (< 100MB)
//   - Simple implementation
//
// Memory Usage:
// Each file gets its own buffer. Total memory = sum of all buffer sizes.
// Buffers are released when Reset() is called or on Close().
//
// Thread Safety:
// Safe for concurrent use. Operations on different content IDs are fully
// parallel. Operations on the same content ID are serialized.
type MemoryWriteCache struct {
	buffers map[string]*buffer
	mu      sync.RWMutex
	closed  bool
	metrics CacheMetrics
}

// NewMemoryWriteCache creates a new in-memory write cache.
//
// Parameters:
//   - metrics: Optional metrics collector (can be nil)
//
// Returns:
//   - WriteCache: New cache instance
func NewMemoryWriteCache(metrics CacheMetrics) WriteCache {
	if metrics == nil {
		metrics = noopCacheMetrics{}
	}

	return &MemoryWriteCache{
		buffers: make(map[string]*buffer),
		closed:  false,
		metrics: metrics,
	}
}

// getOrCreateBuffer retrieves an existing buffer or creates a new one.
//
// This helper reduces code duplication and simplifies locking.
func (c *MemoryWriteCache) getOrCreateBuffer(id metadata.ContentID) (*buffer, error) {
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
func (c *MemoryWriteCache) getBuffer(id metadata.ContentID) (*buffer, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.closed {
		return nil, false
	}

	idStr := string(id)
	buf, exists := c.buffers[idStr]
	return buf, exists
}

// WriteAt writes data at the specified offset for a content ID.
func (c *MemoryWriteCache) WriteAt(id metadata.ContentID, data []byte, offset int64) error {
	start := time.Now()
	defer func() {
		c.metrics.ObserveWrite(int64(len(data)), time.Since(start))
	}()

	if offset < 0 {
		return fmt.Errorf("negative offset: %d", offset)
	}

	buf, err := c.getOrCreateBuffer(id)
	if err != nil {
		return err
	}

	buf.mu.Lock()
	defer buf.mu.Unlock()

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

	// Record cache size after write
	c.metrics.RecordCacheSize(string(id), int64(len(buf.data)))

	return nil
}

// ReadAt reads data from the cache at the specified offset.
func (c *MemoryWriteCache) ReadAt(id metadata.ContentID, buf []byte, offset int64) (int, error) {
	start := time.Now()
	defer func() {
		c.metrics.ObserveRead(int64(len(buf)), time.Since(start))
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

// ReadAll returns all cached data for a content ID.
func (c *MemoryWriteCache) ReadAll(id metadata.ContentID) ([]byte, error) {
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

// Size returns the size of cached data for a content ID.
func (c *MemoryWriteCache) Size(id metadata.ContentID) int64 {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return 0
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	return int64(len(cacheBuf.data))
}

// Reset clears the buffer for a specific content ID.
func (c *MemoryWriteCache) Reset(id metadata.ContentID) error {
	c.mu.Lock()
	defer func() {
		// Record buffer count after reset
		c.metrics.RecordBufferCount(len(c.buffers))
		c.mu.Unlock()
	}()

	if c.closed {
		return fmt.Errorf("cache is closed")
	}

	idStr := string(id)
	buf, exists := c.buffers[idStr]
	if !exists {
		return nil // Already cleared
	}

	// Clear buffer data
	buf.mu.Lock()
	buf.data = nil
	buf.mu.Unlock()

	// Remove from map
	delete(c.buffers, idStr)

	// Record cache reset
	c.metrics.RecordCacheReset(idStr)

	return nil
}

// ResetAll clears all buffers for all content IDs.
func (c *MemoryWriteCache) ResetAll() error {
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

	return nil
}

// LastWrite returns the timestamp of the last write for a content ID.
func (c *MemoryWriteCache) LastWrite(id metadata.ContentID) time.Time {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return time.Time{}
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	return cacheBuf.lastWrite
}

// List returns all content IDs with active buffers.
func (c *MemoryWriteCache) List() []metadata.ContentID {
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

// Close releases all resources and clears all buffers.
func (c *MemoryWriteCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil // Already closed
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
//   - cache: Cache to inspect
//
// Returns:
//   - map: Content ID -> buffer info
func GetInfo(cache WriteCache) map[metadata.ContentID]BufferInfo {
	result := make(map[metadata.ContentID]BufferInfo)

	for _, id := range cache.List() {
		result[id] = BufferInfo{
			Size:      cache.Size(id),
			LastWrite: cache.LastWrite(id),
		}
	}

	return result
}
