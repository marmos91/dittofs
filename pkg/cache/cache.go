// Package cache implements buffering for content stores.
//
// This package provides a generic caching layer that acts as a buffer between
// protocol handlers (NFS) and content stores (S3, filesystem, etc.). The cache
// is intentionally simple - it's just a buffer. Flushing logic is handled by
// the NFS handlers, not the cache itself.
//
// Key Design Principles:
//   - Cache is just a buffer - no flush methods
//   - Supports both full-file Write() and random-access WriteAt()
//   - Context-aware for cancellation and timeouts
//   - Thread-safe for concurrent operations
//   - Tracks total size for eviction logic
//
// The cache enables:
//   - Async write mode: WRITE goes to cache, COMMIT flushes to storage
//   - Incremental multipart uploads: Upload to S3 as cache grows (no full buffering)
//   - Efficient memory usage: Evict flushed files when cache is full
package cache

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// Cache provides a generic buffering layer for content.
//
// The cache is protocol-agnostic and can be used with any content store backend.
// It buffers writes in memory (or disk, depending on implementation) and allows
// reading back that data before it's flushed to the underlying storage.
//
// Separation of Concerns:
// The cache does NOT handle flushing logic. That responsibility belongs to the
// protocol handlers (e.g., NFS COMMIT handler). This keeps the cache interface
// simple and focused on buffering.
//
// Use Cases:
//   - NFS async write mode: Buffer writes, flush on COMMIT
//   - S3 incremental uploads: Buffer until 5MB, upload incrementally
//   - Filesystem optimization: Buffer small writes, flush large batches
//   - Testing: In-memory buffer for fast tests
//
// Thread Safety:
// Implementations must be safe for concurrent use by multiple goroutines.
// Operations on different content IDs should not block each other.
type Cache interface {
	// ====================================================================
	// Write Operations
	// ====================================================================

	// Write replaces the entire content for a content ID.
	//
	// This is a full-file write operation. Any existing cached data for
	// this content ID is completely replaced.
	//
	// Use Cases:
	//   - Initial file creation
	//   - Complete file replacement
	//   - Flushing from another cache
	//
	// For partial updates, use WriteAt instead.
	//
	// Context Cancellation:
	// For large writes, implementations should periodically check context
	// to ensure responsive cancellation.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier
	//   - data: Complete content data
	//
	// Returns:
	//   - error: Returns error if write fails or context is cancelled
	//
	// Example:
	//
	//	err := cache.Write(ctx, contentID, []byte("Hello, World!"))
	//	if err != nil {
	//	    return fmt.Errorf("cache write failed: %w", err)
	//	}
	Write(ctx context.Context, id metadata.ContentID, data []byte) error

	// WriteAt writes data at the specified offset for a content ID.
	//
	// This implements random-access writes for protocols like NFS that
	// write files in arbitrary order. If no cached data exists for this
	// content ID, it's created automatically.
	//
	// Sparse File Behavior:
	//   - If offset > current size, the gap is filled with zeros
	//   - If offset < current size, existing data is overwritten
	//   - If offset == current size, data is appended
	//
	// Context Cancellation:
	// For large writes, implementations should periodically check context
	// to ensure responsive cancellation.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier (created if doesn't exist)
	//   - data: Data to write
	//   - offset: Byte offset where writing begins (0-based)
	//
	// Returns:
	//   - error: Returns error if write fails or context is cancelled
	//
	// Example (NFS WRITE):
	//
	//	// Write 4KB at offset 8192
	//	err := cache.WriteAt(ctx, contentID, data, 8192)
	//	if err != nil {
	//	    return fmt.Errorf("cache write failed: %w", err)
	//	}
	WriteAt(ctx context.Context, id metadata.ContentID, data []byte, offset int64) error

	// ====================================================================
	// Read Operations
	// ====================================================================

	// Read returns all cached data for a content ID.
	//
	// This returns the complete cached data for the content ID. The data
	// is NOT removed from the cache - call Remove() after successful flush.
	//
	// If no cached data exists, returns empty slice and nil error.
	//
	// Context Cancellation:
	// The method checks context before reading. For very large cached data,
	// implementations may check context during the copy operation.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier
	//
	// Returns:
	//   - []byte: All cached data (empty if no data cached)
	//   - error: Returns error if read fails or context is cancelled
	//
	// Example (flushing to store):
	//
	//	data, err := cache.Read(ctx, contentID)
	//	if err != nil {
	//	    return err
	//	}
	//	err = store.WriteContent(ctx, contentID, data)
	//	if err != nil {
	//	    return err
	//	}
	//	cache.Remove(contentID)  // Clear after successful flush
	Read(ctx context.Context, id metadata.ContentID) ([]byte, error)

	// ReadAt reads data from the cache at the specified offset.
	//
	// This implements io.ReaderAt pattern for reading partial cache data.
	// Useful for incremental flushing (e.g., S3 multipart uploads) where
	// you want to read chunks without loading the entire cached file.
	//
	// Semantics follow io.ReaderAt:
	//   - Reads len(buf) bytes into buf starting at offset
	//   - Returns n bytes read and error
	//   - If n < len(buf), error explains why (io.EOF, etc.)
	//   - Does not modify cache state
	//
	// Context Cancellation:
	// The method checks context before reading.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier
	//   - buf: Buffer to read into
	//   - offset: Byte offset to start reading from (0-based)
	//
	// Returns:
	//   - int: Number of bytes read
	//   - error: io.EOF if offset >= size, error on failure
	//
	// Example (reading 5MB chunks for S3 upload):
	//
	//	chunk := make([]byte, 5*1024*1024)
	//	n, err := cache.ReadAt(ctx, contentID, chunk, offset)
	//	if err != nil && err != io.EOF {
	//	    return err
	//	}
	//	// Upload chunk[:n] to S3
	ReadAt(ctx context.Context, id metadata.ContentID, buf []byte, offset int64) (int, error)

	// ====================================================================
	// Metadata
	// ====================================================================

	// Size returns the size of cached data for a content ID.
	//
	// This returns the current size in bytes. Returns 0 if no cached data
	// exists for the content ID.
	//
	// Use Cases:
	//   - Check if cache threshold reached (for flushing)
	//   - Calculate how much to read for multipart upload
	//   - Monitor cache growth
	//
	// Parameters:
	//   - id: Content identifier
	//
	// Returns:
	//   - int64: Size in bytes (0 if no cached data)
	Size(id metadata.ContentID) int64

	// LastWrite returns the timestamp of the last write for a content ID.
	//
	// This is used for timeout-based operations like:
	//   - Auto-flush idle files (last write > 30 seconds ago)
	//   - LRU eviction (evict oldest last-write time)
	//   - Monitoring write patterns
	//
	// Returns zero time if no cached data exists.
	//
	// Parameters:
	//   - id: Content identifier
	//
	// Returns:
	//   - time.Time: Last write timestamp (zero if no cached data)
	LastWrite(id metadata.ContentID) time.Time

	// Exists checks if cached data exists for a content ID.
	//
	// This is a lightweight existence check without reading the data.
	//
	// Parameters:
	//   - id: Content identifier
	//
	// Returns:
	//   - bool: True if cached data exists, false otherwise
	Exists(id metadata.ContentID) bool

	// List returns all content IDs with cached data.
	//
	// This is used for:
	//   - Auto-flush workers iterating over all cached files
	//   - Server shutdown (flush all before exit)
	//   - Monitoring and debugging
	//
	// Returns:
	//   - []metadata.ContentID: List of content IDs with cached data
	List() []metadata.ContentID

	// ====================================================================
	// Cache Management
	// ====================================================================

	// Remove clears the cached data for a specific content ID.
	//
	// This deletes the cached data from the cache. Typically called after
	// successfully flushing to the underlying content store.
	//
	// The operation is idempotent - removing non-existent data succeeds.
	//
	// Parameters:
	//   - id: Content identifier
	//
	// Returns:
	//   - error: Returns error if removal fails (implementation-specific)
	//
	// Example (after flush):
	//
	//	// Flush to store
	//	data, _ := cache.Read(ctx, contentID)
	//	store.WriteContent(ctx, contentID, data)
	//
	//	// Remove from cache
	//	cache.Remove(contentID)
	Remove(id metadata.ContentID) error

	// RemoveAll clears all cached data for all content IDs.
	//
	// This is useful for:
	//   - Server shutdown (after flushing all data)
	//   - Testing (clean slate between tests)
	//   - Emergency cleanup
	//
	// Returns:
	//   - error: Returns error if cleanup fails
	//
	// Example (server shutdown):
	//
	//	// Flush all cached data first
	//	for _, id := range cache.List() {
	//	    // ... flush logic ...
	//	}
	//
	//	// Clear cache
	//	cache.RemoveAll()
	RemoveAll() error

	// ====================================================================
	// Cache Statistics
	// ====================================================================

	// TotalSize returns the total size of all cached data across all files.
	//
	// This is the sum of all cached data sizes. Used for:
	//   - Cache utilization monitoring
	//   - Eviction decisions (is cache full?)
	//   - Memory pressure detection
	//
	// Returns:
	//   - int64: Total cached size in bytes
	TotalSize() int64

	// MaxSize returns the maximum cache size configured for this cache.
	//
	// This is the cache size limit. When TotalSize() approaches MaxSize(),
	// eviction should occur.
	//
	// Returns 0 if there's no size limit (unlimited cache).
	//
	// Returns:
	//   - int64: Maximum cache size in bytes (0 = unlimited)
	MaxSize() int64

	// ====================================================================
	// Lifecycle
	// ====================================================================

	// Close releases all resources and clears all cached data.
	//
	// After Close, the cache cannot be used. All subsequent operations
	// will fail.
	//
	// Typically called during:
	//   - Server shutdown
	//   - Share unmount
	//   - Testing cleanup
	//
	// Returns:
	//   - error: Returns error if cleanup fails
	//
	// Example (server shutdown):
	//
	//	defer cache.Close()
	Close() error
}
