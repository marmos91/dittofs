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

// buffer represents a single file's cache entry.
type buffer struct {
	data      []byte
	lastWrite time.Time
	mu        sync.Mutex

	// State tracking (unified cache)
	state         cache.CacheState
	flushedOffset uint64
	lastAccess    time.Time

	// Validity tracking (read cache coherency)
	// cachedMtime and cachedSize are valid when state == StateCached
	cachedMtime time.Time
	cachedSize  uint64

	// Prefetch support
	prefetchedOffset uint64 // How many bytes have been prefetched (only valid in StatePrefetching)
	prefetchExpected uint64 // Expected total size of the file being prefetched
}

// hasMetadata returns true if cached metadata has been set.
// Metadata is set when SetCachedMetadata is called with a non-zero mtime.
func (b *buffer) hasMetadata() bool {
	return !b.cachedMtime.IsZero()
}

// MemoryCache manages in-memory buffers for multiple files.
//
// Thread Safety:
// Two-level locking is used for efficiency:
//   - c.mu (RWMutex): Protects the buffers map structure
//   - buf.mu (Mutex): Protects individual buffer content and state
//
// This allows concurrent operations on different files without contention.
type MemoryCache struct {
	buffers   map[string]*buffer
	mu        sync.RWMutex
	closed    bool
	maxSize   uint64             // Maximum total cache size (0 = unlimited)
	totalSize atomic.Uint64      // Current total cache size (atomic for lock-free access)
	metrics   cache.CacheMetrics // Optional metrics collector
}

// NewMemoryCache creates a new in-memory cache with the specified size limit.
//
// Parameters:
//   - maxSize: Maximum total cache size in bytes. Use 0 for unlimited cache.
//   - metrics: Optional metrics collector for cache statistics. Can be nil.
//
// The cache starts empty and grows as data is written. When maxSize is exceeded,
// LRU eviction removes the least recently accessed clean entries (dirty entries
// are protected from eviction).
func NewMemoryCache(maxSize uint64, metrics cache.CacheMetrics) cache.Cache {
	return &MemoryCache{
		buffers: make(map[string]*buffer),
		closed:  false,
		maxSize: maxSize,
		metrics: metrics,
	}
}

// getOrCreateBuffer retrieves an existing buffer or creates a new one.
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
		c.mu.Lock()
		// Double-check after acquiring write lock (prevents race condition)
		buf, exists = c.buffers[idStr]
		if !exists {
			now := time.Now()
			buf = &buffer{
				data:       make([]byte, 0, defaultBufferCapacity),
				lastWrite:  now,
				lastAccess: now,
				state:      cache.StateBuffering,
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

	buf, exists := c.buffers[string(id)]
	return buf, exists
}

// Write replaces the entire cached content for a content ID.
//
// This operation:
//   - Creates a new cache entry if one doesn't exist
//   - Replaces all existing data with the new data
//   - Updates lastWrite and lastAccess timestamps to now
//   - Transitions from StateCached to StateBuffering (invalidating cached metadata)
//   - Triggers LRU eviction if the cache size limit would be exceeded
//
// Returns an error if the context is cancelled or the cache is closed.
func (c *MemoryCache) Write(ctx context.Context, id metadata.ContentID, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.ensureCacheSize(uint64(len(data)))

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
	oldSize := uint64(len(buf.data))

	buf.data = make([]byte, len(data))
	copy(buf.data, data)

	now := time.Now()
	buf.lastWrite = now
	buf.lastAccess = now

	if buf.state == cache.StateCached {
		buf.state = cache.StateBuffering
		buf.flushedOffset = 0
		// Clear cached metadata - new write invalidates previous cache
		buf.cachedMtime = time.Time{}
		buf.cachedSize = 0
	}

	newSize := uint64(len(buf.data))
	buf.mu.Unlock()

	// Update total size atomically
	if newSize > oldSize {
		c.totalSize.Add(newSize - oldSize)
	} else if oldSize > newSize {
		c.totalSize.Add(^(oldSize - newSize - 1))
	}

	if c.metrics != nil {
		c.metrics.RecordCacheSize(string(id), int64(newSize))
		c.metrics.RecordTotalCacheSize(int64(c.totalSize.Load()))
	}

	return nil
}

// WriteAt writes data at the specified byte offset for a content ID.
//
// This operation:
//   - Creates a new cache entry if one doesn't exist
//   - Extends the buffer if writing past the current end
//   - Fills any gap between current size and offset with zeros (sparse file semantics)
//   - Updates lastWrite and lastAccess timestamps to now
//   - Transitions from StateCached to StateBuffering (invalidating cached metadata)
//   - Triggers LRU eviction if the cache size limit would be exceeded
//
// Buffer Growth Strategy:
//   - Doubles capacity for buffers < 100MB
//   - Grows by max(25%, 20MB) for larger buffers
//
// Returns an error if the context is cancelled or the cache is closed.
func (c *MemoryCache) WriteAt(ctx context.Context, id metadata.ContentID, data []byte, offset uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.ensureCacheSize(uint64(len(data)))

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

	oldSize := uint64(len(buf.data))
	currentSize := oldSize
	writeEnd := offset + uint64(len(data))

	// Extend buffer if needed
	if writeEnd > currentSize {
		newSize := writeEnd

		if newSize > uint64(cap(buf.data)) {
			newCap := uint64(cap(buf.data))
			if newCap == 0 {
				newCap = defaultBufferCapacity
			}

			// Growth strategy: double for < 100MB, then grow by max(25%, 20MB)
			for newCap < newSize {
				if newCap < 100*1024*1024 {
					newCap *= 2
				} else {
					growth := newCap / 4
					if growth < 20*1024*1024 {
						growth = 20 * 1024 * 1024
					}
					newCap += growth
				}
			}

			newBuf := make([]byte, newSize, newCap)
			copy(newBuf, buf.data)
			buf.data = newBuf
		} else {
			buf.data = buf.data[:newSize]
		}

		// Fill gap with zeros (sparse file)
		if offset > currentSize {
			for i := currentSize; i < offset; i++ {
				buf.data[i] = 0
			}
		}
	}

	copy(buf.data[offset:], data)

	now := time.Now()
	buf.lastWrite = now
	buf.lastAccess = now

	if buf.state == cache.StateCached {
		buf.state = cache.StateBuffering
		buf.flushedOffset = 0
		// Clear cached metadata - new write invalidates previous cache
		buf.cachedMtime = time.Time{}
		buf.cachedSize = 0
	}
	newSize := uint64(len(buf.data))
	buf.mu.Unlock()

	if newSize > oldSize {
		c.totalSize.Add(newSize - oldSize)
	}

	if c.metrics != nil {
		c.metrics.RecordCacheSize(string(id), int64(newSize))
		c.metrics.RecordTotalCacheSize(int64(c.totalSize.Load()))
	}

	return nil
}

// Read returns a copy of all cached data for a content ID.
//
// Returns an independent copy of the data, so modifications to the returned
// slice do not affect the cached data.
//
// Returns:
//   - Empty slice (not nil) if the content ID doesn't exist
//   - Error if context is cancelled
func (c *MemoryCache) Read(ctx context.Context, id metadata.ContentID) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return []byte{}, nil
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	result := make([]byte, len(cacheBuf.data))
	copy(result, cacheBuf.data)

	return result, nil
}

// ReadAt reads data from the cache at the specified byte offset into buf.
//
// Follows io.ReaderAt semantics:
//   - Returns number of bytes read and any error
//   - Returns io.EOF if offset is at or past end of data
//   - Returns partial read with io.EOF if fewer bytes available than len(buf)
//   - Updates lastAccess timestamp on successful read
//
// Returns:
//   - (0, io.EOF) if content ID doesn't exist or offset >= size
//   - (n, io.EOF) if n < len(buf) bytes were available
//   - (len(buf), nil) if full buffer was read
//   - (0, err) if context is cancelled
func (c *MemoryCache) ReadAt(ctx context.Context, id metadata.ContentID, buf []byte, offset uint64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	start := time.Now()
	defer func() {
		if c.metrics != nil {
			c.metrics.ObserveRead(int64(len(buf)), time.Since(start))
		}
	}()

	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return 0, io.EOF
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	if offset >= uint64(len(cacheBuf.data)) {
		return 0, io.EOF
	}

	cacheBuf.lastAccess = time.Now()

	n := copy(buf, cacheBuf.data[offset:])
	if n < len(buf) {
		return n, io.EOF
	}

	return n, nil
}

// Size returns the size in bytes of cached data for a content ID.
//
// Returns 0 if the content ID doesn't exist.
func (c *MemoryCache) Size(id metadata.ContentID) uint64 {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return 0
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	return uint64(len(cacheBuf.data))
}

// LastWrite returns the timestamp of the last write operation for a content ID.
//
// Returns zero time if the content ID doesn't exist.
func (c *MemoryCache) LastWrite(id metadata.ContentID) time.Time {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return time.Time{}
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	return cacheBuf.lastWrite
}

// Exists checks if a cache entry exists for the given content ID.
//
// Note: Returns true even for empty entries (size 0).
func (c *MemoryCache) Exists(id metadata.ContentID) bool {
	_, exists := c.getBuffer(id)
	return exists
}

// List returns all content IDs currently in the cache.
//
// Returns an empty slice if the cache is empty or closed.
// The order of IDs is not guaranteed.
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

// Remove deletes the cache entry for a specific content ID.
//
// This operation:
//   - Removes the entry from the cache
//   - Updates totalSize to reflect freed memory
//   - Records metrics if configured
//
// Idempotent: Returns nil if the content ID doesn't exist.
// Returns an error only if the cache is closed.
func (c *MemoryCache) Remove(id metadata.ContentID) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return fmt.Errorf("cache is closed")
	}

	idStr := string(id)
	buf, exists := c.buffers[idStr]

	if !exists {
		return nil
	}

	buf.mu.Lock()
	bufSize := uint64(len(buf.data))
	buf.mu.Unlock()

	if bufSize > 0 {
		c.totalSize.Add(^(bufSize - 1))
	}

	delete(c.buffers, idStr)

	if c.metrics != nil {
		c.metrics.RecordCacheReset(idStr)
		c.metrics.RecordBufferCount(len(c.buffers))
		c.metrics.RecordTotalCacheSize(int64(c.totalSize.Load()))
	}

	return nil
}

// RemoveAll deletes all entries from the cache.
//
// Resets totalSize to 0 and creates a fresh empty buffers map.
// Returns an error only if the cache is closed.
func (c *MemoryCache) RemoveAll() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return fmt.Errorf("cache is closed")
	}

	c.buffers = make(map[string]*buffer)
	c.totalSize.Store(0)

	if c.metrics != nil {
		c.metrics.RecordBufferCount(0)
		c.metrics.RecordTotalCacheSize(0)
	}

	return nil
}

// TotalSize returns the total size in bytes of all cached data.
//
// This is an atomic read and does not require locking.
func (c *MemoryCache) TotalSize() uint64 {
	return c.totalSize.Load()
}

// MaxSize returns the maximum cache size limit in bytes.
//
// Returns 0 if the cache is unlimited.
func (c *MemoryCache) MaxSize() uint64 {
	return c.maxSize
}

// Close releases all cache resources and marks the cache as closed.
//
// After Close:
//   - All buffers are released (set to nil)
//   - TotalSize is reset to 0
//   - All operations return errors
//
// Idempotent: Calling Close multiple times is safe and returns nil.
func (c *MemoryCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}

	c.buffers = nil
	c.totalSize.Store(0)
	c.closed = true

	return nil
}

// EvictLRU evicts least recently accessed clean entries until cache size <= targetSize.
//
// Parameters:
//   - targetSize: Target cache size in bytes. If 0, uses 90% of MaxSize.
//
// Eviction Rules:
//   - Only clean entries (StateCached) can be evicted
//   - Dirty entries (StateBuffering, StateUploading) are protected
//   - Entries are evicted in order of lastAccess (oldest first)
//
// Returns:
//   - count: Number of entries evicted
//   - bytesFreed: Total bytes freed
//   - (0, 0) if cache is unlimited (MaxSize == 0), closed, or already at target
func (c *MemoryCache) EvictLRU(targetSize uint64) (int, uint64) {
	if c.maxSize == 0 {
		return 0, 0
	}

	if targetSize == 0 {
		targetSize = (c.maxSize * 90) / 100
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return 0, 0
	}

	currentSize := c.totalSize.Load()
	if currentSize <= targetSize {
		return 0, 0
	}

	type evictionCandidate struct {
		id         string
		size       uint64
		lastAccess time.Time
	}

	candidates := make([]evictionCandidate, 0, len(c.buffers))
	for idStr, buf := range c.buffers {
		buf.mu.Lock()
		if !buf.state.IsDirty() {
			candidates = append(candidates, evictionCandidate{
				id:         idStr,
				size:       uint64(len(buf.data)),
				lastAccess: buf.lastAccess,
			})
		}
		buf.mu.Unlock()
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].lastAccess.Before(candidates[j].lastAccess)
	})

	evicted := 0
	var bytesFreed uint64

	for _, candidate := range candidates {
		if currentSize <= targetSize {
			break
		}

		buf, exists := c.buffers[candidate.id]
		if !exists {
			continue
		}

		buf.mu.Lock()
		if buf.state.IsDirty() {
			buf.mu.Unlock()
			continue
		}
		bufSize := uint64(len(buf.data))
		buf.mu.Unlock()

		delete(c.buffers, candidate.id)

		c.totalSize.Add(^(bufSize - 1))

		currentSize -= bufSize
		bytesFreed += bufSize
		evicted++

		if c.metrics != nil {
			c.metrics.RecordCacheReset(candidate.id)
		}
	}

	if c.metrics != nil {
		c.metrics.RecordBufferCount(len(c.buffers))
		c.metrics.RecordTotalCacheSize(int64(c.totalSize.Load()))
	}

	return evicted, bytesFreed
}

// ensureCacheSize checks if adding dataSize bytes would exceed MaxSize().
func (c *MemoryCache) ensureCacheSize(dataSize uint64) {
	if c.maxSize == 0 {
		return
	}

	totalSize := c.TotalSize()
	if totalSize+dataSize <= c.maxSize {
		return
	}

	hysteresisTarget := (c.maxSize * 90) / 100
	var targetSize uint64
	if hysteresisTarget > dataSize {
		targetSize = hysteresisTarget - dataSize
	}

	_, _ = c.EvictLRU(targetSize)
}

// ============================================================================
// State Management
// ============================================================================

// GetState returns the current state of a cache entry.
//
// Returns StateNone if the content ID doesn't exist.
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
// No-op if the content ID doesn't exist.
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
// Used by incremental uploads to track progress. Returns 0 if the content ID
// doesn't exist or hasn't been flushed yet.
func (c *MemoryCache) GetFlushedOffset(id metadata.ContentID) uint64 {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return 0
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	return cacheBuf.flushedOffset
}

// SetFlushedOffset updates the flushed byte offset for a cache entry.
//
// Used by incremental uploads to record progress. No-op if the content ID
// doesn't exist.
func (c *MemoryCache) SetFlushedOffset(id metadata.ContentID, offset uint64) {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	cacheBuf.flushedOffset = offset
}

// ============================================================================
// Cache Coherency
// ============================================================================

// GetCachedMetadata returns the metadata snapshot stored for cache validation.
//
// Returns:
//   - mtime: The modification time when data was cached
//   - size: The file size when data was cached
//   - ok: True if metadata was set via SetCachedMetadata, false otherwise
//
// Returns (zero, 0, false) if the content ID doesn't exist or metadata wasn't set.
func (c *MemoryCache) GetCachedMetadata(id metadata.ContentID) (mtime time.Time, size uint64, ok bool) {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return time.Time{}, 0, false
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	if !cacheBuf.hasMetadata() {
		return time.Time{}, 0, false
	}

	return cacheBuf.cachedMtime, cacheBuf.cachedSize, true
}

// SetCachedMetadata stores a metadata snapshot for cache coherency validation.
//
// This should be called after successfully caching data to record the file's
// mtime and size at that moment. Subsequent reads can use IsValid to check
// if the cached data is still current.
//
// No-op if the content ID doesn't exist.
func (c *MemoryCache) SetCachedMetadata(id metadata.ContentID, mtime time.Time, size uint64) {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	cacheBuf.cachedMtime = mtime
	cacheBuf.cachedSize = size
}

// IsValid checks if cached data is still valid against current file metadata.
//
// Validation Rules:
//   - Returns false if content ID doesn't exist
//   - Returns true if entry is dirty (StateBuffering or StateUploading)
//   - Returns false if no cached metadata was set
//   - Returns true if currentMtime and currentSize match cached values
//   - Returns false if either mtime or size has changed
//
// Use this to check cache coherency before serving reads from cache.
func (c *MemoryCache) IsValid(id metadata.ContentID, currentMtime time.Time, currentSize uint64) bool {
	cacheBuf, exists := c.getBuffer(id)
	if !exists {
		return false
	}

	cacheBuf.mu.Lock()
	defer cacheBuf.mu.Unlock()

	if cacheBuf.state.IsDirty() {
		return true
	}

	if !cacheBuf.hasMetadata() {
		return false
	}

	if !cacheBuf.cachedMtime.Equal(currentMtime) || cacheBuf.cachedSize != currentSize {
		return false
	}

	return true
}

// LastAccess returns the timestamp of the last read or write access.
//
// Used by LRU eviction to determine which entries to remove first.
// Returns zero time if the content ID doesn't exist.
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
	Size      uint64
	LastWrite time.Time
}

// GetInfo returns diagnostic information about all cache entries.
//
// This is a helper function for debugging and monitoring. It returns
// the size and last write time for every cached content ID.
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

// ============================================================================
// Prefetch Support
// ============================================================================

// StartPrefetch initiates a background prefetch operation for a content ID.
//
// Creates a new cache entry in StatePrefetching state with pre-allocated
// capacity for expectedSize bytes.
//
// Returns:
//   - true if prefetch was started successfully
//   - false if cache is closed or entry already exists with non-None state
//
// After calling StartPrefetch, use WriteAt to stream data and SetPrefetchedOffset
// to update progress. Call CompletePrefetch when done.
func (c *MemoryCache) StartPrefetch(id metadata.ContentID, expectedSize uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return false
	}

	idStr := string(id)
	buf, exists := c.buffers[idStr]

	if exists {
		buf.mu.Lock()
		defer buf.mu.Unlock()

		if buf.state != cache.StateNone {
			return false
		}
	}

	now := time.Now()
	buf = &buffer{
		data:             make([]byte, 0, expectedSize),
		lastWrite:        now,
		lastAccess:       now,
		state:            cache.StatePrefetching,
		prefetchedOffset: 0,
		prefetchExpected: expectedSize,
	}

	c.buffers[idStr] = buf
	return true
}

// WaitForPrefetchOffset blocks until the prefetch has reached the required byte offset.
//
// Polls every 10ms until one of:
//   - Prefetched offset >= requiredOffset (returns nil)
//   - Entry transitions to StateCached (prefetch complete, returns nil)
//   - Entry is removed or transitions to unexpected state (returns error)
//   - Context is cancelled (returns ctx.Err())
//
// Use this to wait for specific bytes to be available during streaming prefetch.
func (c *MemoryCache) WaitForPrefetchOffset(ctx context.Context, id metadata.ContentID, requiredOffset uint64) error {
	const pollInterval = 10 * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		buf, exists := c.getBuffer(id)
		if !exists {
			return fmt.Errorf("prefetch entry removed")
		}

		buf.mu.Lock()
		state := buf.state
		prefetchedOffset := buf.prefetchedOffset
		buf.mu.Unlock()

		if state != cache.StatePrefetching {
			if state == cache.StateCached {
				return nil
			}
			return fmt.Errorf("prefetch failed: entry in state %s", state)
		}

		if prefetchedOffset >= requiredOffset {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// GetPrefetchedOffset returns how many bytes have been prefetched so far.
//
// Only valid when entry is in StatePrefetching state.
// Returns 0 if content ID doesn't exist or is not being prefetched.
func (c *MemoryCache) GetPrefetchedOffset(id metadata.ContentID) uint64 {
	buf, exists := c.getBuffer(id)
	if !exists {
		return 0
	}

	buf.mu.Lock()
	defer buf.mu.Unlock()

	if buf.state != cache.StatePrefetching {
		return 0
	}

	return buf.prefetchedOffset
}

// SetPrefetchedOffset updates the prefetched byte offset to signal progress.
//
// Only updates if:
//   - Entry exists and is in StatePrefetching state
//   - New offset is greater than current prefetched offset (monotonic increase)
//
// Call this after writing each chunk during prefetch to allow waiting readers
// to proceed as soon as their required bytes are available.
func (c *MemoryCache) SetPrefetchedOffset(id metadata.ContentID, offset uint64) {
	buf, exists := c.getBuffer(id)
	if !exists {
		return
	}

	buf.mu.Lock()
	defer buf.mu.Unlock()

	if buf.state != cache.StatePrefetching {
		return
	}

	if offset > buf.prefetchedOffset {
		buf.prefetchedOffset = offset
	}
}

// CompletePrefetch finalizes a prefetch operation.
//
// If success is true:
//   - Transitions entry from StatePrefetching to StateCached
//   - Entry is ready to serve reads
//
// If success is false:
//   - Removes the entry from cache entirely
//   - Frees all associated memory
//
// Call this when the prefetch background task completes (either successfully
// or due to an error).
func (c *MemoryCache) CompletePrefetch(id metadata.ContentID, success bool) {
	if !success {
		c.mu.Lock()
		idStr := string(id)
		buf, exists := c.buffers[idStr]

		if exists {
			buf.mu.Lock()
			bufSize := uint64(len(buf.data))
			buf.state = cache.StateNone
			buf.mu.Unlock()

			delete(c.buffers, idStr)

			if bufSize > 0 {
				c.totalSize.Add(^(bufSize - 1))
			}
		}
		c.mu.Unlock()
		return
	}

	buf, exists := c.getBuffer(id)
	if !exists {
		return
	}

	buf.mu.Lock()
	defer buf.mu.Unlock()

	if buf.state == cache.StatePrefetching {
		buf.state = cache.StateCached
		buf.prefetchedOffset = uint64(len(buf.data))
	}
}
