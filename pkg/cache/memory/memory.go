// Package memory implements an in-memory cache for content stores.
//
// This package provides a fast in-memory buffer for caching file content
// before it's flushed to persistent storage backends.
package memory

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// Default buffer capacity per file (5MB - matches S3 multipart threshold)
const defaultBufferCapacity = 5 * 1024 * 1024

// ============================================================================
// MemoryCache - In-memory implementation
// ============================================================================

// buffer represents a single file's cache entry.
type buffer struct {
	data      []byte
	lastWrite time.Time
	mu        sync.Mutex

	// State tracking (unified cache)
	state         cache.CacheState
	flushedOffset int64
	lastAccess    time.Time
	cachedAt      time.Time

	// Validity tracking (read cache coherency)
	cachedMtime time.Time
	cachedSize  uint64
	hasMetadata bool // true if cachedMtime/cachedSize are set
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
// Total size is tracked with atomic operations to avoid lock contention.
type MemoryCache struct {
	buffers   map[string]*buffer
	mu        sync.RWMutex
	closed    bool
	maxSize   int64              // Maximum total cache size (0 = unlimited)
	totalSize atomic.Int64       // Current total cache size (atomic for lock-free access)
	metrics   cache.CacheMetrics // Optional metrics collector
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
			now := time.Now()
			buf = &buffer{
				data:       make([]byte, 0, defaultBufferCapacity),
				lastWrite:  now,
				lastAccess: now,
				cachedAt:   now,
				state:      cache.StateBuffering, // New entries start buffering
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

	// Debug: Log write start
	// fmt.Printf("[CACHE] Write start: id=%s size=%d maxSize=%d totalSize=%d\n",
	//	string(id), len(data), c.maxSize, c.totalSize.Load())

	// Ensure cache has space before writing (triggers eviction if needed)
	c.ensureCacheSize(int64(len(data)))

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
	oldSize := int64(len(buf.data))
	// Replace entire buffer
	buf.data = make([]byte, len(data))
	copy(buf.data, data)
	now := time.Now()
	buf.lastWrite = now
	buf.lastAccess = now
	// Reset to buffering state on new write (if was Cached)
	if buf.state == cache.StateCached {
		buf.state = cache.StateBuffering
		buf.flushedOffset = 0
		buf.hasMetadata = false
	}
	newSize := int64(len(buf.data))
	buf.mu.Unlock()

	// Update total size atomically (delta = newSize - oldSize)
	sizeDelta := newSize - oldSize
	if sizeDelta != 0 {
		c.totalSize.Add(sizeDelta)
	}

	// Record cache size after write
	if c.metrics != nil {
		c.metrics.RecordCacheSize(string(id), newSize)
		c.metrics.RecordTotalCacheSize(c.totalSize.Load())
	}

	// Debug: Log write complete
	// fmt.Printf("[CACHE] Write done: id=%s newSize=%d totalSize=%d\n",
	//	string(id), newSize, c.totalSize.Load())

	return nil
}

// WriteAt writes data at the specified offset for a content ID.
func (c *MemoryCache) WriteAt(ctx context.Context, id metadata.ContentID, data []byte, offset int64) error {
	// Check context before starting
	if err := ctx.Err(); err != nil {
		return err
	}

	// Ensure cache has space before writing (triggers eviction if needed)
	// Note: This is an estimate - actual size increase may be less if overwriting
	c.ensureCacheSize(int64(len(data)))

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

	// Acquire lock for the write operation
	// We used to try optimizing by checking capacity before locking, but this caused:
	// 1. Data races when copying old data without lock
	// 2. Incorrect totalSize tracking from stale oldSize
	// 3. Double-lock overhead
	// Simpler is better: just do everything in one lock acquisition
	buf.mu.Lock()

	// Capture size BEFORE the write for accurate delta tracking
	oldSize := int64(len(buf.data))
	currentSize := oldSize
	writeEnd := offset + int64(len(data))

	// Extend buffer if needed
	if writeEnd > currentSize {
		newSize := writeEnd

		if newSize > int64(cap(buf.data)) {
			// Need to reallocate - calculate new capacity
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
			// Just extend length (capacity is sufficient)
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
	now := time.Now()
	buf.lastWrite = now
	buf.lastAccess = now
	// Reset to buffering state on new write (if was Cached)
	if buf.state == cache.StateCached {
		buf.state = cache.StateBuffering
		buf.flushedOffset = 0
		buf.hasMetadata = false
	}
	newSize := int64(len(buf.data))
	buf.mu.Unlock()

	// Update total size atomically (delta = newSize - oldSize)
	sizeDelta := newSize - oldSize
	if sizeDelta != 0 {
		c.totalSize.Add(sizeDelta)
	}

	// Record cache size after write
	if c.metrics != nil {
		c.metrics.RecordCacheSize(string(id), newSize)
		c.metrics.RecordTotalCacheSize(c.totalSize.Load())
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

	// Update last access time on read
	cacheBuf.lastAccess = time.Now()

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

// Remove clears the cached data for a specific content ID.
func (c *MemoryCache) Remove(id metadata.ContentID) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return fmt.Errorf("cache is closed")
	}

	idStr := string(id)
	buf, exists := c.buffers[idStr]
	if !exists {
		fmt.Printf("[CACHE] Remove: id=%s not found (already removed)\n", string(id))
		return nil // Already removed (idempotent)
	}

	fmt.Printf("[CACHE] Remove: acquiring buf lock for id=%s\n", string(id))
	// Get buffer size before clearing
	buf.mu.Lock()
	bufSize := int64(len(buf.data))
	buf.data = nil
	buf.mu.Unlock()
	fmt.Printf("[CACHE] Remove: released buf lock for id=%s size=%d\n", string(id), bufSize)

	// Update total size atomically
	if bufSize > 0 {
		c.totalSize.Add(-bufSize)
	}

	// Remove from map
	delete(c.buffers, idStr)

	// Record metrics after removal
	if c.metrics != nil {
		c.metrics.RecordCacheReset(idStr)
		c.metrics.RecordBufferCount(len(c.buffers))
		c.metrics.RecordTotalCacheSize(c.totalSize.Load())
	}

	fmt.Printf("[CACHE] Remove DONE: id=%s totalSize=%d\n", string(id), c.totalSize.Load())
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

	// Reset total size atomically
	c.totalSize.Store(0)

	// Record metrics after clearing
	if c.metrics != nil {
		c.metrics.RecordBufferCount(0)
		c.metrics.RecordTotalCacheSize(0)
	}

	return nil
}

// TotalSize returns the total size of all cached data across all files.
// This is now lock-free and uses atomic operations for zero contention.
func (c *MemoryCache) TotalSize() int64 {
	return c.totalSize.Load()
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

// EvictLRU evicts the least recently used (oldest by last write time) cached files
// until the total cache size drops below the target threshold.
//
// This method is called automatically when writes would exceed MaxSize(), but can
// also be called manually to free up cache space.
//
// Eviction strategy:
//   - Only evicts if MaxSize() is configured (> 0)
//   - Evicts oldest files first (by LastWrite timestamp)
//   - Continues evicting until TotalSize() <= targetSize
//   - Default targetSize is 90% of MaxSize() to avoid thrashing
//   - Thread-safe: can be called concurrently with other operations
//
// Parameters:
//   - targetSize: Target cache size in bytes (0 = use 90% of MaxSize())
//
// Returns:
//   - int: Number of files evicted
//   - int64: Total bytes freed
func (c *MemoryCache) EvictLRU(targetSize int64) (int, int64) {
	// No eviction if cache has no size limit
	if c.maxSize == 0 {
		return 0, 0
	}

	// Use 90% of max size as default target (hysteresis to avoid thrashing)
	if targetSize == 0 {
		targetSize = (c.maxSize * 90) / 100
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return 0, 0
	}

	// Check if eviction is needed using atomic total
	currentSize := c.totalSize.Load()
	if currentSize <= targetSize {
		return 0, 0 // No eviction needed
	}

	// Build list of candidates sorted by last write time (oldest first)
	type evictionCandidate struct {
		id        string
		size      int64
		lastWrite time.Time
	}

	candidates := make([]evictionCandidate, 0, len(c.buffers))
	for idStr, buf := range c.buffers {
		buf.mu.Lock()
		candidates = append(candidates, evictionCandidate{
			id:        idStr,
			size:      int64(len(buf.data)),
			lastWrite: buf.lastWrite,
		})
		buf.mu.Unlock()
	}

	// Sort by last write time (oldest first) using standard library
	// O(n log n) is more efficient than selection sort O(n²) for larger caches
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].lastWrite.Before(candidates[j].lastWrite)
	})

	// Evict oldest files until we reach target size
	evicted := 0
	bytesFreed := int64(0)

	for _, candidate := range candidates {
		if currentSize <= targetSize {
			break // Target reached
		}

		// Remove the buffer
		buf, exists := c.buffers[candidate.id]
		if !exists {
			continue // Already removed by concurrent operation
		}

		buf.mu.Lock()
		bufSize := int64(len(buf.data))
		buf.data = nil
		buf.mu.Unlock()

		delete(c.buffers, candidate.id)

		// Update total size atomically
		c.totalSize.Add(-bufSize)

		currentSize -= bufSize
		bytesFreed += bufSize
		evicted++

		// Record cache removal
		if c.metrics != nil {
			c.metrics.RecordCacheReset(candidate.id)
		}
	}

	// Update metrics after eviction
	if c.metrics != nil {
		c.metrics.RecordBufferCount(len(c.buffers))
		c.metrics.RecordTotalCacheSize(c.totalSize.Load())
	}

	return evicted, bytesFreed
}

// ensureCacheSize checks if adding dataSize bytes would exceed MaxSize(),
// and evicts LRU entries if needed.
//
// This should be called before adding new data to the cache.
// It's a helper to automatically trigger eviction during Write/WriteAt operations.
//
// Parameters:
//   - dataSize: Number of bytes about to be added
//
// Thread safety: Caller must NOT hold c.mu lock
func (c *MemoryCache) ensureCacheSize(dataSize int64) {
	// No eviction if cache has no size limit
	if c.maxSize == 0 {
		return
	}

	// Check if we need to evict using atomic total (lock-free check)
	totalSize := c.TotalSize()
	if totalSize+dataSize <= c.maxSize {
		return // No eviction needed
	}

	// Need to evict - target should leave room for the incoming write
	// We want: (size after eviction) + dataSize <= 90% of max
	// Therefore: targetSize = (maxSize * 90 / 100) - dataSize
	// But ensure target is at least 0
	hysteresisTarget := (c.maxSize * 90) / 100
	targetSize := hysteresisTarget - dataSize
	if targetSize < 0 {
		targetSize = 0
	}

	evicted, bytesFreed := c.EvictLRU(targetSize)

	// Log eviction activity (only in debug mode)
	_ = evicted
	_ = bytesFreed
	// Note: Removed logger.Debug() to avoid import dependency
	// Can be re-added if logger is needed for debugging
}

// ============================================================================
// State Management (Unified Cache)
// ============================================================================

// GetState returns the current state of a cache entry.
//
// Returns cache.StateNone if the entry does not exist.
// This is the zero value of CacheState, making it safe to use without
// checking existence first:
//
//	state := c.GetState(id)
//	if state == cache.StateNone {
//	    // Entry doesn't exist, handle accordingly
//	}
func (c *MemoryCache) GetState(id metadata.ContentID) cache.CacheState {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return cache.StateNone
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	return cacheBuf.state
}

// SetState updates the state of a cache entry.
//
// This is a no-op if the entry doesn't exist. Use Write/WriteAt to create
// entries (which start in StateBuffering).
//
// Valid state transitions:
//   - StateBuffering → StateUploading (when flush starts)
//   - StateUploading → StateCached (when finalization completes)
//   - StateCached → StateBuffering (automatically on new Write/WriteAt)
//
// Note: Transitioning to StateNone should use Remove() instead.
func (c *MemoryCache) SetState(id metadata.ContentID, state cache.CacheState) {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	cacheBuf.state = state
}

// GetFlushedOffset returns how many bytes have been flushed to the content store.
//
// Returns 0 if the entry doesn't exist or no data has been flushed yet.
//
// Use this to calculate unflushed data:
//
//	unflushed := cache.Size(id) - cache.GetFlushedOffset(id)
func (c *MemoryCache) GetFlushedOffset(id metadata.ContentID) int64 {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return 0
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	return cacheBuf.flushedOffset
}

// SetFlushedOffset updates the flushed offset for a cache entry.
//
// Called by the flush coordinator after successfully flushing data to the
// content store. The offset should only increase (flushing is forward-only).
//
// This is a no-op if the entry doesn't exist.
func (c *MemoryCache) SetFlushedOffset(id metadata.ContentID, offset int64) {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	cacheBuf.flushedOffset = offset
}

// ============================================================================
// Cache Coherency (Read Validation)
// ============================================================================

// GetCachedMetadata returns the metadata snapshot stored when the entry was cached.
//
// Returns ok=false if:
//   - The entry doesn't exist
//   - No metadata has been stored yet (entry is dirty from writes)
//
// Use this with IsValid() to check if cached data is still fresh.
func (c *MemoryCache) GetCachedMetadata(id metadata.ContentID) (mtime time.Time, size uint64, ok bool) {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return time.Time{}, 0, false
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	if !cacheBuf.hasMetadata {
		return time.Time{}, 0, false
	}

	return cacheBuf.cachedMtime, cacheBuf.cachedSize, true
}

// SetCachedMetadata stores a metadata snapshot for cache validation.
//
// Call this:
//   - After populating cache from a READ (store current file mtime/size)
//   - After finalization (store post-write mtime/size)
//
// This is a no-op if the entry doesn't exist.
func (c *MemoryCache) SetCachedMetadata(id metadata.ContentID, mtime time.Time, size uint64) {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	cacheBuf.cachedMtime = mtime
	cacheBuf.cachedSize = size
	cacheBuf.hasMetadata = true
}

// IsValid checks if cached data is still valid against current file metadata.
//
// Validation logic:
//   - Returns false if entry doesn't exist
//   - Returns true if entry is dirty (Buffering or Uploading) - we're the source of truth
//   - Returns false if no cached metadata exists
//   - Returns false if mtime or size differs from current file metadata
//
// Example usage in READ handler:
//
//	if cache.Exists(id) && cache.IsValid(id, file.Mtime, file.Size) {
//	    // Serve from cache
//	} else {
//	    // Fetch from content store, populate cache
//	}
func (c *MemoryCache) IsValid(id metadata.ContentID, currentMtime time.Time, currentSize uint64) bool {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return false
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	// Dirty entries are always valid (we're the source of truth)
	if cacheBuf.state.IsDirty() {
		return true
	}

	// Clean entries: validate against current metadata
	if !cacheBuf.hasMetadata {
		return false
	}

	// Check if file was modified externally
	if !cacheBuf.cachedMtime.Equal(currentMtime) || cacheBuf.cachedSize != currentSize {
		return false
	}

	return true
}

// LastAccess returns the timestamp of the last access (read or write).
//
// Returns zero time if the entry doesn't exist.
//
// Used for LRU eviction - entries with oldest LastAccess are evicted first.
func (c *MemoryCache) LastAccess(id metadata.ContentID) time.Time {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return time.Time{}
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	return cacheBuf.lastAccess
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
