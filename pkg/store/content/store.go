package content

import (
	"context"
	"io"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// ============================================================================
// ContentStore Interface
// ============================================================================

// ContentStore provides protocol-agnostic content management for file data storage.
//
// This interface is designed to abstract away the underlying storage mechanism
// (filesystem, S3, memory, etc.) and provide a consistent API for file content
// operations. Protocol handlers interact with metadata via MetadataStore and
// file content via ContentStore.
//
// Separation of Concerns:
//
// The content store manages only the raw file data (bytes). It does NOT manage:
//   - File metadata (attributes, permissions, ownership) → handled by MetadataStore
//   - File hierarchy and directory structure → handled by MetadataStore
//   - Access control and permissions → handled by MetadataStore
//   - File handles and path resolution → handled by MetadataStore
//
// Content Coordination:
// The MetadataStore and ContentStore are designed to work together:
//   - Metadata contains ContentID that references content
//   - Protocol handlers use ContentID to read/write content
//   - Garbage collection removes content not referenced by metadata
//
// This separation allows:
//   - Independent scaling of metadata and content storage
//   - Content deduplication (multiple files sharing same ContentID)
//   - Flexible storage backends (local disk, S3, distributed storage)
//   - Different storage tiers (hot/cold storage, SSD/HDD)
//
// Design Principles:
//   - Storage-agnostic: Works with filesystem, S3, memory, distributed storage
//   - Capability-based: Interfaces for optional features (seeking, multipart, streaming)
//   - Consistent error handling: All operations return well-defined errors
//   - Context-aware: All operations respect context cancellation and timeouts
//   - No access control: Content store trusts ContentID from metadata layer
//
// Content Identifiers:
// ContentID is an opaque string identifier for content. The format is
// implementation-specific:
//   - Filesystem: Random UUID (e.g., "550e8400-e29b-41d4-a716-446655440000")
//   - S3: Object key (e.g., "content/550e8400-e29b-41d4-a716-446655440000")
//   - Memory: Random ID or hash
//   - Content-addressable: SHA256 hash of content
//
// The ContentID must be unique within the content store and should be treated
// as opaque by callers. Only the content store implementation interprets the ID.
//
// Thread Safety:
// Implementations must be safe for concurrent use by multiple goroutines.
// Concurrent writes to the same ContentID may result in undefined behavior
// (last-write-wins, corruption, or error) depending on the implementation.
// Callers should use external synchronization when concurrent access to the
// same content is needed.
type ContentStore interface {
	// ========================================================================
	// Content Reading
	// ========================================================================

	// ReadContent returns a reader for the content identified by the given ID.
	//
	// The returned reader provides sequential access to the content data.
	// The caller is responsible for closing the reader when done.
	//
	// For random access reads, use SeekableContentStore.ReadContentSeekable()
	// if available. For partial reads, implementations may support ReadAt()
	// operations on the returned reader (if it implements io.ReaderAt).
	//
	// Context Cancellation:
	// The method checks context before opening content. Once the reader is
	// returned, callers should monitor context and close the reader if
	// cancelled.
	//
	// Large Content:
	// For very large content, callers should use streaming reads with periodic
	// context checks to ensure responsive cancellation.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier to read
	//
	// Returns:
	//   - io.ReadCloser: Reader for the content (must be closed by caller)
	//   - error: ErrContentNotFound if content doesn't exist, or context/IO errors
	//
	// Example:
	//
	//	reader, err := store.ReadContent(ctx, contentID)
	//	if err != nil {
	//	    return err
	//	}
	//	defer reader.Close()
	//
	//	data, err := io.ReadAll(reader)
	//	if err != nil {
	//	    return err
	//	}
	ReadContent(ctx context.Context, id metadata.ContentID) (io.ReadCloser, error)

	// GetContentSize returns the size of the content in bytes.
	//
	// This is a lightweight operation that returns content size without reading
	// the data. Useful for:
	//   - NFS GETATTR operations (file size)
	//   - HTTP Content-Length headers
	//   - Quota checks before reading
	//   - Buffer allocation sizing
	//
	// Context Cancellation:
	// The method checks context before performing the size lookup.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier
	//
	// Returns:
	//   - uint64: Size of the content in bytes
	//   - error: ErrContentNotFound if content doesn't exist, or context/IO errors
	GetContentSize(ctx context.Context, id metadata.ContentID) (uint64, error)

	// ContentExists checks if content with the given ID exists.
	//
	// This is a lightweight existence check that doesn't read content data
	// or metadata. Useful for:
	//   - Garbage collection (checking if content is orphaned)
	//   - Validation before operations
	//   - Health checks
	//
	// Context Cancellation:
	// The method checks context before performing the existence check.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier to check
	//
	// Returns:
	//   - bool: True if content exists, false otherwise
	//   - error: Only returns error for context cancellation or storage access
	//     failures, NOT for non-existent content (returns false, nil in that case)
	ContentExists(ctx context.Context, id metadata.ContentID) (bool, error)

	// ========================================================================
	// Storage Information
	// ========================================================================

	// GetStorageStats returns statistics about the content storage.
	//
	// This provides information about storage capacity, usage, and health.
	// The information is dynamic and may change as content is added/removed.
	//
	// Use Cases:
	//   - Capacity planning and monitoring
	//   - Quota enforcement
	//   - Health checks and alerting
	//   - Administrative dashboards
	//
	// Implementation Notes:
	// For some backends (S3, distributed storage), gathering stats may be
	// expensive. Implementations should consider caching stats with TTL.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//
	// Returns:
	//   - *StorageStats: Current storage statistics
	//   - error: Only context cancellation or storage access errors
	GetStorageStats(ctx context.Context) (*StorageStats, error)
}

// ============================================================================
// WritableContentStore Interface
// ============================================================================

// WritableContentStore extends ContentStore with write and delete operations.
//
// This interface adds the ability to create, modify, and delete content.
// Not all content stores support writing (e.g., read-only mirrors, archives).
//
// Write Semantics:
//   - WriteAt: Partial writes at specific offsets (for NFS WRITE operations)
//   - Truncate: Resize content (for NFS SETATTR size changes)
//   - Delete: Remove content (for file deletion, garbage collection)
//   - WriteContent: Convenience method for full content writes
//
// Atomicity:
// Implementations should strive for atomic operations where possible:
//   - WriteAt should be atomic for the written region
//   - Truncate should be atomic
//   - Delete should be idempotent (deleting non-existent content succeeds)
//
// Concurrent Writes:
// Concurrent writes to the same ContentID have undefined behavior:
//   - May result in corruption (interleaved writes)
//   - May result in last-write-wins
//   - May return an error
//
// Callers should use external synchronization (locks in MetadataStore)
// to prevent concurrent writes to the same content.
type WritableContentStore interface {
	ContentStore

	// ========================================================================
	// Content Writing
	// ========================================================================

	// WriteAt writes data at the specified offset.
	//
	// This implements partial file updates for NFS WRITE operations. The content
	// will be created if it doesn't exist. If the offset is beyond the current
	// content size, the gap is filled with zeros (sparse file behavior).
	//
	// Sparse File Support:
	// Implementations should support sparse files where possible:
	//   - Writing at offset > size should not allocate intermediate space
	//   - Reads from unallocated regions return zeros
	//   - GetContentSize returns logical size (including holes)
	//
	// Context Cancellation:
	// For large writes (>1MB), implementations should periodically check
	// context to ensure responsive cancellation.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier (created if doesn't exist)
	//   - data: Data to write
	//   - offset: Byte offset where writing begins
	//
	// Returns:
	//   - error: Returns error if write fails or context is cancelled
	//
	// Example (NFS WRITE handler):
	//
	//	// Write 4KB at offset 8192
	//	err := store.WriteAt(ctx, contentID, data, 8192)
	//	if err != nil {
	//	    return nfs.NFS3ErrIO
	//	}
	WriteAt(ctx context.Context, id metadata.ContentID, data []byte, offset int64) error

	// Truncate changes the size of the content.
	//
	// This implements file size changes for NFS SETATTR operations:
	//   - If newSize < currentSize: Content is truncated (data removed)
	//   - If newSize > currentSize: Content is extended (zeros added)
	//   - If newSize == currentSize: No-op (succeeds immediately)
	//
	// Sparse File Behavior:
	// When extending (newSize > currentSize), implementations should:
	//   - Not allocate space for the extended region (if sparse supported)
	//   - Reads from extended region return zeros
	//   - GetContentSize returns the new logical size
	//
	// Context Cancellation:
	// The method checks context before performing the truncate operation.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier
	//   - newSize: New size in bytes
	//
	// Returns:
	//   - error: ErrContentNotFound if content doesn't exist, or context/IO errors
	//
	// Example (NFS SETATTR size change):
	//
	//	// Truncate file to 1024 bytes
	//	err := store.Truncate(ctx, contentID, 1024)
	//	if err != nil {
	//	    return nfs.NFS3ErrIO
	//	}
	Truncate(ctx context.Context, id metadata.ContentID, newSize uint64) error

	// Delete removes content from the store.
	//
	// This removes all data associated with the ContentID. The operation is
	// idempotent - deleting non-existent content returns nil (success).
	//
	// Idempotency Rationale:
	// Idempotent deletion simplifies error handling:
	//   - Retries are safe after network failures
	//   - Concurrent deletions don't cause errors
	//   - Garbage collection can retry without checks
	//
	// Storage Reclamation:
	// The operation should reclaim storage space immediately or mark content
	// for asynchronous deletion. The space may not be immediately available
	// depending on the backend:
	//   - Filesystem: Space reclaimed immediately (OS handles)
	//   - S3: Space reclaimed after DELETE completes
	//   - Distributed storage: May require background cleanup
	//
	// Context Cancellation:
	// The method checks context before performing deletion. For large content
	// or slow backends, cancellation may occur during the delete operation.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier to delete
	//
	// Returns:
	//   - error: Only returns error for context cancellation or storage failures,
	//     NOT for non-existent content (returns nil in that case)
	//
	// Example (file deletion):
	//
	//	// Delete content after removing metadata
	//	err := store.Delete(ctx, contentID)
	//	if err != nil {
	//	    // Log but don't fail - can be garbage collected later
	//	    logger.Warn("Failed to delete content: %v", err)
	//	}
	Delete(ctx context.Context, id metadata.ContentID) error

	// WriteContent writes the entire content in one operation.
	//
	// This is a convenience method for writing complete content in one call.
	// Useful for:
	//   - Testing and setup
	//   - Small files that fit in memory
	//   - Content creation (not updates)
	//
	// For large files or partial updates, use WriteAt instead.
	//
	// If content with this ID already exists, it is overwritten (replaced).
	//
	// Context Cancellation:
	// For large content (>10MB), implementations should use chunked writes
	// with periodic context checks for responsive cancellation.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier (created if doesn't exist, replaced if exists)
	//   - data: Complete content data
	//
	// Returns:
	//   - error: Returns error if write fails or context is cancelled
	//
	// Example (testing):
	//
	//	// Create test content
	//	contentID := metadata.ContentID("test-content-123")
	//	err := store.WriteContent(ctx, contentID, []byte("Hello, World!"))
	//	if err != nil {
	//	    t.Fatalf("Failed to write content: %v", err)
	//	}
	WriteContent(ctx context.Context, id metadata.ContentID, data []byte) error
}

// ============================================================================
// FlushableContentStore Interface
// ============================================================================

// FlushableContentStore extends WritableContentStore with explicit write flushing.
//
// This interface is OPTIONAL. Content stores that buffer writes in memory or
// temporary storage should implement this interface to allow protocol handlers
// to explicitly flush writes to durable storage.
//
// Design Rationale:
// Many storage backends benefit from write buffering:
//   - S3: Buffer writes to avoid read-modify-write cycles
//   - Filesystem: Write-back caching for performance
//   - Network stores: Batch writes to reduce round-trips
//
// However, NFS protocol requires explicit durability guarantees:
//   - WRITE with FILE_SYNC: Must persist before returning
//   - WRITE with UNSTABLE: Can buffer, but COMMIT must flush
//   - File close: Should flush (though NFS has no explicit close)
//
// Use Cases:
//   - S3: Accumulate sequential writes, upload in one PutObject or multipart
//   - Filesystem: Sync() to ensure data reaches disk
//   - Memory: No-op (already durable in RAM)
//
// Protocol Integration:
//   - NFS WRITE with FILE_SYNC/DATA_SYNC calls FlushWrites() after WriteAt()
//   - NFS COMMIT calls FlushWrites() to persist unstable writes
//   - NFS handlers should call FlushWrites() before file close
//   - Stores that don't buffer can return nil immediately (no-op)
//
// Thread Safety:
// Implementations must be safe for concurrent calls across different content IDs.
// Flushing one file should not block operations on other files.
//
// Platform Considerations:
//   - macOS NFS client never sends COMMIT - auto-flush timeout required!
//   - Linux may or may not send COMMIT depending on mount options
//   - Windows behavior varies by NFS client implementation
//
// Therefore, implementations should also support timeout-based auto-flush
// to ensure durability even when COMMIT is not called.
type FlushableContentStore interface {
	WritableContentStore

	// FlushWrites forces all buffered writes for a content ID to durable storage.
	//
	// This method blocks until:
	//   - All buffered data is written to the backing store
	//   - The backing store acknowledges durability (e.g., S3 PutObject completes)
	//
	// Behavior:
	//   - Idempotent: Multiple calls are safe, subsequent calls are no-ops
	//   - Per-file: Only flushes the specified content ID
	//   - Clears cache: After successful flush, internal buffers should be released
	//   - Partial flush: If cache exceeds threshold (e.g., 5MB for S3 multipart),
	//     implementation may flush incrementally and keep session active
	//
	// Error Handling:
	//   - On success: Buffer is cleared, returns nil
	//   - On failure: Buffer is retained for retry, returns error
	//
	// Context Cancellation:
	// The method should respect context cancellation and stop flushing.
	// Partial flushes (e.g., multipart uploads) should be aborted cleanly.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier to flush
	//
	// Returns:
	//   - error: Returns error if flush fails (write buffer retained for retry)
	//
	// Example (NFS WRITE handler with stable=FILE_SYNC):
	//
	//	// Check if store supports flushing
	//	type flusher interface {
	//	    FlushWrites(ctx context.Context, id metadata.ContentID) error
	//	}
	//
	//	if flushable, ok := contentStore.(flusher); ok {
	//	    if err := flushable.FlushWrites(ctx, contentID); err != nil {
	//	        return nfs.NFS3ErrIO
	//	    }
	//	}
	FlushWrites(ctx context.Context, id metadata.ContentID) error

	// FlushThreshold returns the recommended flush size for this store.
	//
	// Protocol handlers should check the cache size against this threshold
	// and call FlushWrites() when the threshold is reached. This allows
	// each content store to define its own optimal flush point:
	//   - S3: 5MB (multipart upload threshold)
	//   - Filesystem: 1MB (reasonable memory limit)
	//   - Memory: 0 (no threshold, unlimited buffering)
	//
	// How it works:
	//   1. Handler writes to cache
	//   2. Handler checks: if cache.Size(id) >= store.FlushThreshold() { flush }
	//   3. Content store handles flush logic (multipart vs simple upload)
	//
	// Returns:
	//   - uint64: Threshold in bytes (0 = no threshold, unlimited buffering)
	//
	// Example (NFS WRITE handler):
	//
	//	// After writing to cache
	//	cache.WriteAt(contentID, data, offset)
	//
	//	// Check threshold
	//	if flushable, ok := store.(FlushableContentStore); ok {
	//	    if uint64(cache.Size(contentID)) >= flushable.FlushThreshold() {
	//	        flushable.FlushWrites(ctx, contentID)
	//	    }
	//	}
	FlushThreshold() uint64
}

// ============================================================================
// CloseableContentStore Interface
// ============================================================================

// CloseableContentStore allows explicit closing of open write sessions.
//
// This interface is OPTIONAL. Some content stores may maintain open file handles,
// multipart upload sessions, or other resources that benefit from explicit cleanup.
//
// Design Rationale:
// NFS protocol does not have explicit file close operations - the protocol is
// stateless. However, internally we may track resources per file:
//   - Write caches (memory or temp files)
//   - Multipart upload sessions (S3)
//   - Open file descriptors
//   - Locks or other resources
//
// This interface provides a hint to the content store that:
//   - The file handle is no longer in use
//   - Resources can be cleaned up
//   - Any pending writes should be flushed
//
// Use Cases:
//   - Metadata store calls CloseContent() when last file handle is released
//   - Server shutdown calls CloseContent() for all open files
//   - Periodic cleanup of idle resources
//
// Protocol Integration:
//   - When NFS file handle is released (reference count reaches 0)
//   - During server graceful shutdown
//   - When idle timeout is reached
//
// Relationship with FlushWrites:
// CloseContent typically calls FlushWrites internally, plus additional cleanup:
//   - Flush any buffered writes
//   - Delete temporary files
//   - Complete or abort multipart uploads
//   - Close file descriptors
//   - Release locks
//
// Thread Safety:
// Implementations must be safe for concurrent calls. Multiple calls to
// CloseContent for the same ID should be idempotent.
type CloseableContentStore interface {
	WritableContentStore

	// CloseContent closes any open resources for a content ID.
	//
	// For stores with write buffering, this typically:
	//   1. Calls FlushWrites() to persist data
	//   2. Releases temporary resources (temp files, upload sessions)
	//   3. Clears internal state
	//
	// This is a hint for cleanup - stores must handle cases where
	// CloseContent is never called (e.g., server crash).
	//
	// Idempotency:
	// Multiple calls with the same ID are safe. Subsequent calls are no-ops.
	//
	// Error Handling:
	// If flush fails, the error is returned and resources are retained for retry.
	// If only cleanup fails (after successful flush), the error may be logged
	// but should not prevent future operations on the content ID.
	//
	// Context Cancellation:
	// The method should respect context cancellation. If cancelled during flush,
	// resources are retained and can be closed later.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier to close
	//
	// Returns:
	//   - error: Returns error if close fails (typically from FlushWrites failure)
	//
	// Example (metadata store on handle release):
	//
	//	// When last reference to file handle is released
	//	type closeable interface {
	//	    CloseContent(ctx context.Context, id metadata.ContentID) error
	//	}
	//
	//	if closeableStore, ok := contentStore.(closeable); ok {
	//	    if err := closeableStore.CloseContent(ctx, contentID); err != nil {
	//	        logger.Warn("Failed to close content: %v", err)
	//	        // Don't fail handle release - can retry later
	//	    }
	//	}
	CloseContent(ctx context.Context, id metadata.ContentID) error
}

// ============================================================================
// SeekableContentStore Interface
// ============================================================================

// SeekableContentStore is an optional interface for random access reads.
//
// This interface is supported by storage backends that allow efficient
// random access:
//   - Filesystem: Supports efficient seeking (✓)
//   - Memory: Supports efficient seeking (✓)
//   - S3: Does NOT support seeking (✗) - requires range requests
//
// Use Cases:
//   - Efficient partial reads (read middle of file)
//   - Reading file in non-sequential order
//   - Implementing database-like access patterns
//
// Protocol handlers should check if the content store implements this
// interface and use it when available for better performance.
type SeekableContentStore interface {
	ContentStore

	// ReadContentSeekable returns a reader with seeking support.
	//
	// The returned reader implements io.ReadSeekCloser, allowing random
	// access to any position in the content without reading from the beginning.
	//
	// This is more efficient than ReadContent() when:
	//   - Reading small portions of large files
	//   - Reading in non-sequential order
	//   - Implementing random access protocols
	//
	// The caller is responsible for closing the reader when done.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier to read
	//
	// Returns:
	//   - io.ReadSeekCloser: Seekable reader (must be closed by caller)
	//   - error: ErrContentNotFound if content doesn't exist, or context/IO errors
	//
	// Example (random access read):
	//
	//	reader, err := store.ReadContentSeekable(ctx, contentID)
	//	if err != nil {
	//	    return err
	//	}
	//	defer reader.Close()
	//
	//	// Read last 100 bytes
	//	_, err = reader.Seek(-100, io.SeekEnd)
	//	if err != nil {
	//	    return err
	//	}
	//	tail, err := io.ReadAll(reader)
	ReadContentSeekable(ctx context.Context, id metadata.ContentID) (io.ReadSeekCloser, error)
}

// ============================================================================
// ReadAtContentStore Interface
// ============================================================================

// ReadAtContentStore is an optional interface for efficient random-access reads.
//
// This interface enables efficient partial reads using the ReaderAt pattern,
// which is critical for performance when serving protocols like NFS that
// request small chunks of data from large files.
//
// Performance Benefits:
//   - S3: Uses byte-range requests instead of downloading entire objects
//   - Filesystem: Can use pread() syscall for efficient positioned reads
//   - Network: Reduces bandwidth usage by only transferring requested bytes
//
// Use Cases:
//   - NFS READ operations (typically 4-64KB chunks)
//   - Random access patterns
//   - Large files where full download is wasteful
//
// Implementations:
//   - S3: MUST implement for acceptable performance (✓)
//   - Filesystem: Should implement for efficiency (can implement)
//   - Memory: Optional (less critical for in-memory storage)
//
// Example (NFS READ handler):
//
//	if readAtStore, ok := contentRepo.(content.ReadAtContentStore); ok {
//	    // Use efficient range read
//	    buf := make([]byte, count)
//	    n, err := readAtStore.ReadAt(ctx, contentID, buf, offset)
//	} else {
//	    // Fall back to sequential read (less efficient)
//	    reader, err := contentRepo.ReadContent(ctx, contentID)
//	    // ... seek and read ...
//	}
type ReadAtContentStore interface {
	ContentStore

	// ReadAt reads len(p) bytes into p starting at offset in the content.
	//
	// ReadAt follows io.ReaderAt semantics:
	//   - Returns n bytes read and error
	//   - If n < len(p), error explains why (io.EOF, io.ErrUnexpectedEOF, etc.)
	//   - Can be called concurrently with different offsets
	//   - Does not affect or use any implicit file position
	//
	// For S3 backends, this uses HTTP Range requests:
	//   GET /bucket/key
	//   Range: bytes=offset-end
	//
	// This is dramatically more efficient than ReadContent() for partial reads:
	//   - ReadContent: Downloads entire 100MB file for 4KB request
	//   - ReadAt: Downloads only the requested 4KB (25,000x less data!)
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier to read from
	//   - p: Buffer to read into (len(p) bytes will be read)
	//   - offset: Byte offset to start reading from (0-based)
	//
	// Returns:
	//   - n: Number of bytes read (may be less than len(p) on error or EOF)
	//   - error: io.EOF if offset is at/past end, or other read errors
	//
	// Thread Safety:
	//   - Safe for concurrent calls with different offsets
	//   - Each call is independent
	ReadAt(ctx context.Context, id metadata.ContentID, p []byte, offset int64) (n int, err error)
}

// ============================================================================
// StreamingContentStore Interface
// ============================================================================

// StreamingContentStore is an optional interface for streaming operations.
//
// This interface provides more control over reading and writing compared to
// the base ContentStore interface. It's particularly useful for:
//   - S3: Efficient streaming uploads/downloads
//   - Large files: Avoid loading entire file in memory
//   - Progressive processing: Process data as it's read/written
//
// Implementations:
//   - Filesystem: Can implement using os.File (✓)
//   - Memory: Can implement using bytes.Buffer wrappers (✓)
//   - S3: Should implement for efficient streaming (✓)
type StreamingContentStore interface {
	ContentStore

	// OpenWriter returns a writer for streaming content writes.
	//
	// This allows writing content incrementally without loading it all in
	// memory first. The writer must be closed to finalize the write.
	//
	// Write Semantics:
	//   - Sequential writes only (no seeking)
	//   - Content is created if it doesn't exist
	//   - If content exists, it is overwritten (replaced)
	//   - Closing the writer commits the content
	//   - Not closing the writer may lose data
	//
	// Error Handling:
	// Errors during Write() should be returned immediately. Errors during
	// Close() indicate the content was not successfully written.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier (created if doesn't exist, replaced if exists)
	//
	// Returns:
	//   - io.WriteCloser: Writer for streaming writes (must be closed)
	//   - error: Returns error if writer cannot be created
	//
	// Example (streaming upload):
	//
	//	writer, err := store.OpenWriter(ctx, contentID)
	//	if err != nil {
	//	    return err
	//	}
	//	defer writer.Close()
	//
	//	// Stream data from source
	//	_, err = io.Copy(writer, sourceReader)
	//	if err != nil {
	//	    return err
	//	}
	//
	//	// Commit by closing
	//	if err := writer.Close(); err != nil {
	//	    return err
	//	}
	OpenWriter(ctx context.Context, id metadata.ContentID) (io.WriteCloser, error)

	// OpenReader returns a reader for streaming content reads.
	//
	// This is similar to ReadContent() but may provide additional control
	// or different behavior depending on the implementation.
	//
	// Some implementations may return the same reader as ReadContent().
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier to read
	//
	// Returns:
	//   - io.ReadCloser: Reader for streaming reads (must be closed)
	//   - error: ErrContentNotFound if content doesn't exist, or context/IO errors
	OpenReader(ctx context.Context, id metadata.ContentID) (io.ReadCloser, error)
}

// ============================================================================
// MultipartContentStore Interface
// ============================================================================

// MultipartContentStore is an optional interface for multipart uploads.
//
// This interface is designed for S3-style multipart uploads, which are essential
// for uploading large files efficiently:
//   - Files can be uploaded in parallel parts
//   - Failed parts can be retried without re-uploading entire file
//   - Supports very large files (>5GB)
//   - Efficient bandwidth usage
//
// Multipart Upload Flow:
//  1. BeginMultipartUpload() → uploadID
//  2. UploadPart() for each part (can be parallel)
//  3. CompleteMultipartUpload() to finalize
//  4. OR AbortMultipartUpload() to cancel
//
// Implementations:
//   - S3: Native multipart upload support (✓)
//   - Filesystem: Can implement by assembling parts (✓)
//   - Memory: Probably not useful (✗)
//
// Part Size Guidelines:
//   - Minimum part size: 5MB (except last part)
//   - Maximum part size: 5GB
//   - Maximum parts: 10,000
//   - Optimal part size: 10-100MB depending on network
type MultipartContentStore interface {
	ContentStore

	// BeginMultipartUpload initiates a multipart upload session.
	//
	// This creates an upload session and returns an upload ID that must be
	// used for all subsequent part uploads and completion.
	//
	// The upload is not finalized until CompleteMultipartUpload() is called.
	// Incomplete uploads should be cleaned up using AbortMultipartUpload()
	// or a background cleanup process.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier for the content being uploaded
	//
	// Returns:
	//   - string: Upload ID for this multipart upload session
	//   - error: Returns error if upload cannot be initiated
	//
	// Example:
	//
	//	uploadID, err := store.BeginMultipartUpload(ctx, contentID)
	//	if err != nil {
	//	    return err
	//	}
	BeginMultipartUpload(ctx context.Context, id metadata.ContentID) (uploadID string, err error)

	// UploadPart uploads one part of a multipart upload.
	//
	// Parts can be uploaded in parallel from multiple goroutines. Part numbers
	// must be unique within an upload (1-10000). Parts can be uploaded in any
	// order.
	//
	// Part Size:
	//   - Minimum size: 5MB (except last part which can be smaller)
	//   - Maximum size: 5GB
	//   - All parts except last should be same size for optimal performance
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier
	//   - uploadID: Upload ID from BeginMultipartUpload
	//   - partNumber: Part number (1-10000, must be unique)
	//   - data: Part data (typically 5MB-5GB)
	//
	// Returns:
	//   - error: Returns error if upload fails
	//
	// Example (parallel upload):
	//
	//	var wg sync.WaitGroup
	//	for i, chunk := range chunks {
	//	    wg.Add(1)
	//	    go func(partNum int, data []byte) {
	//	        defer wg.Done()
	//	        err := store.UploadPart(ctx, contentID, uploadID, partNum, data)
	//	        if err != nil {
	//	            log.Printf("Part %d failed: %v", partNum, err)
	//	        }
	//	    }(i+1, chunk)
	//	}
	//	wg.Wait()
	UploadPart(ctx context.Context, id metadata.ContentID, uploadID string, partNumber int, data []byte) error

	// CompleteMultipartUpload finalizes a multipart upload.
	//
	// This assembles all uploaded parts into the final content. After this
	// operation succeeds, the content is available for reading and the upload
	// ID is no longer valid.
	//
	// Requirements:
	//   - All parts must be successfully uploaded before calling this
	//   - Part numbers must be provided in order (1, 2, 3, ...)
	//   - No gaps in part numbers
	//
	// If completion fails, the upload remains in progress and can be retried
	// or aborted.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier
	//   - uploadID: Upload ID from BeginMultipartUpload
	//   - partNumbers: Ordered list of part numbers to assemble
	//
	// Returns:
	//   - error: Returns error if completion fails
	//
	// Example:
	//
	//	// After all parts uploaded successfully
	//	partNumbers := []int{1, 2, 3, 4, 5}
	//	err := store.CompleteMultipartUpload(ctx, contentID, uploadID, partNumbers)
	//	if err != nil {
	//	    return err
	//	}
	CompleteMultipartUpload(ctx context.Context, id metadata.ContentID, uploadID string, partNumbers []int) error

	// AbortMultipartUpload cancels an in-progress multipart upload.
	//
	// This cleans up all uploaded parts and invalidates the upload ID.
	// Storage space used by parts is reclaimed.
	//
	// This operation is idempotent - aborting a non-existent upload succeeds.
	//
	// Use Cases:
	//   - Upload errors occurred (network failure, data corruption)
	//   - Client cancelled the upload
	//   - Timeout exceeded
	//   - Cleanup of stale uploads
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier
	//   - uploadID: Upload ID from BeginMultipartUpload
	//
	// Returns:
	//   - error: Returns error only for context cancellation or storage failures,
	//     NOT for non-existent uploads
	//
	// Example:
	//
	//	uploadID, _ := store.BeginMultipartUpload(ctx, contentID)
	//	defer func() {
	//	    if err != nil {
	//	        // Cleanup on error
	//	        store.AbortMultipartUpload(context.Background(), contentID, uploadID)
	//	    }
	//	}()
	AbortMultipartUpload(ctx context.Context, id metadata.ContentID, uploadID string) error
}

// ============================================================================
// GarbageCollectableStore Interface
// ============================================================================

// GarbageCollectableStore is an optional interface for content cleanup.
//
// This interface enables automatic garbage collection of unreferenced content.
// Content becomes garbage when:
//   - File is deleted (metadata removed, content remains)
//   - Hard link is removed (last reference to content)
//   - Failed operations leave orphaned content
//   - Migration or sync leaves old content
//
// Garbage Collection Process:
//  1. MetadataStore provides list of all referenced ContentIDs
//  2. ContentStore.ListAllContent() returns all ContentIDs
//  3. Compute: unreferenced = all content - referenced
//  4. ContentStore.DeleteBatch() removes unreferenced content
//
// Implementations:
//   - Filesystem: Should implement (✓)
//   - Memory: Should implement (✓)
//   - S3: Should implement (✓)
type GarbageCollectableStore interface {
	ContentStore

	// ListAllContent returns all content IDs in the store.
	//
	// This returns a complete list of all content currently stored, regardless
	// of whether it's referenced by metadata or not.
	//
	// Performance Warning:
	// For large content stores, this may be slow and consume significant memory.
	// Implementations should consider:
	//   - Pagination or streaming results
	//   - Caching the list (if content changes infrequently)
	//   - Background processing
	//
	// Context Cancellation:
	// For large stores, implementations should periodically check context
	// during iteration.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//
	// Returns:
	//   - []metadata.ContentID: List of all content IDs
	//   - error: Returns error for context cancellation or storage failures
	//
	// Example (garbage collection):
	//
	//	// Get all content
	//	allContent, err := store.ListAllContent(ctx)
	//	if err != nil {
	//	    return err
	//	}
	//
	//	// Get referenced content from metadata
	//	referenced := metadataStore.GetAllContentIDs(ctx)
	//
	//	// Find unreferenced content
	//	unreferenced := difference(allContent, referenced)
	ListAllContent(ctx context.Context) ([]metadata.ContentID, error)

	// DeleteBatch removes multiple content items in one operation.
	//
	// This is more efficient than calling Delete() multiple times, especially
	// for backends like S3 that support batch operations.
	//
	// The operation is best-effort:
	//   - Partial failures are allowed
	//   - Successfully deleted items are not rolled back on partial failure
	//   - Returns map of failed deletions (empty map = all succeeded)
	//
	// Idempotency:
	// Non-existent content IDs are considered successful deletions (not errors).
	//
	// Context Cancellation:
	// For large batches, implementations should periodically check context.
	// If cancelled, returns errors for remaining items.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - ids: Content identifiers to delete
	//
	// Returns:
	//   - map[metadata.ContentID]error: Map of failed deletions (empty = all succeeded)
	//   - error: Returns error only for context cancellation or catastrophic failures,
	//     NOT for individual item failures (those go in the map)
	//
	// Example (garbage collection):
	//
	//	// Delete unreferenced content
	//	failures, err := store.DeleteBatch(ctx, unreferencedIDs)
	//	if err != nil {
	//	    return fmt.Errorf("batch delete failed: %w", err)
	//	}
	//
	//	// Log individual failures
	//	for id, err := range failures {
	//	    logger.Warn("Failed to delete %s: %v", id, err)
	//	}
	//
	//	deletedCount := len(unreferencedIDs) - len(failures)
	//	logger.Info("Garbage collection: deleted %d items, %d failed",
	//	    deletedCount, len(failures))
	DeleteBatch(ctx context.Context, ids []metadata.ContentID) (failures map[metadata.ContentID]error, err error)
}

// ============================================================================
// Supporting Types
// ============================================================================

// StorageStats contains statistics about content storage.
//
// This provides information about storage capacity, usage, and health.
// Different backends may support different fields (unsupported fields
// should be set to 0).
type StorageStats struct {
	// TotalSize is the total storage capacity in bytes.
	// For cloud storage (S3), this may be unlimited (set to MaxUint64).
	// For filesystem, this is the total disk size.
	TotalSize uint64

	// UsedSize is the actual space consumed by content in bytes.
	// This is the sum of all content sizes.
	UsedSize uint64

	// AvailableSize is the remaining available space in bytes.
	// For cloud storage, this may be unlimited (set to MaxUint64).
	// For filesystem: AvailableSize = TotalSize - UsedSize (approximately)
	AvailableSize uint64

	// ContentCount is the total number of content items stored.
	ContentCount uint64

	// AverageSize is the average size of content items in bytes.
	// Calculated as: UsedSize / ContentCount
	// Set to 0 if ContentCount is 0.
	AverageSize uint64
}
