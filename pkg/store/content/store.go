package content

import (
	"context"
	"io"

	"github.com/marmos91/dittofs/pkg/cache"
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
//
// Write Semantics:
// All content stores in DittoFS are writable. The interface includes both
// read and write operations. Optional interfaces (FlushableContentStore,
// ReadAtContentStore, etc.) extend this base interface with additional capabilities.
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
	WriteAt(ctx context.Context, id metadata.ContentID, data []byte, offset uint64) error

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
	//   - Flushing cache to content store
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
	// Example (cache flush):
	//
	//	// Flush cache to content store
	//	data, _ := cache.Read(ctx, contentID)
	//	err := store.WriteContent(ctx, contentID, data)
	//	if err != nil {
	//	    return err
	//	}
	WriteContent(ctx context.Context, id metadata.ContentID, data []byte) error
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
	ReadAt(ctx context.Context, id metadata.ContentID, p []byte, offset uint64) (n int, err error)
}

// ============================================================================
// IncrementalWriteStore Interface
// ============================================================================

// IncrementalWriteStore enables incremental flushing from cache to content store.
//
// This interface is designed for S3-backed stores where uploading large files
// in one operation is not feasible due to:
//   - Single PutObject size limits (varies by S3 service)
//   - Network timeout constraints (3+ minutes for 1GB upload)
//   - NFS client timeouts ("server not responding")
//
// Design: Parallel Multipart Uploads
//
// The implementation uses parallel part uploads for maximum throughput:
//   - Part numbers are deterministic: partNumber = (offset / partSize) + 1
//   - Multiple COMMITs can upload different parts simultaneously
//   - No intermediate buffering - reads directly from cache
//   - Small files (< partSize) use simple PutObject on finalization
//
// S3 Multipart Requirements:
//   - Minimum part size: 5MB (except last part)
//   - Maximum parts: 10,000
//   - Parts can be uploaded in any order (numbered 1, 2, 3, ...)
//
// Flow:
//  1. FlushIncremental(contentID, cache) → uploads complete parts in parallel
//     - Returns 0 if cacheSize < partSize (small file, wait for finalization)
//     - Lazily creates multipart upload on first actual part upload
//  2. CompleteIncrementalWrite(contentID, cache) → finalizes the upload
//     - Small files: uses PutObject directly from cache
//     - Large files: uploads remaining parts + CompleteMultipartUpload
//
// Benefits:
//   - Parallel uploads: Multiple COMMITs upload different parts simultaneously
//   - No blocking: S3 uploads happen outside of locks
//   - No wasted API calls: CreateMultipartUpload only when data >= partSize
//   - Small file optimization: Single PutObject instead of 3-call multipart
//   - Memory efficient: No intermediate buffer, reads directly from cache
//
// Implementations:
//   - S3: MUST implement using native multipart uploads (✓)
//   - Filesystem: No-op (writes are already incremental)
//   - Memory: No-op (no size/timeout constraints)
type IncrementalWriteStore interface {
	ContentStore

	// BeginIncrementalWrite initiates an incremental write session.
	//
	// This creates an S3 multipart upload session and prepares for incremental
	// flushing of cached data. The session is tracked by content ID.
	//
	// Multiple calls with the same ID should be idempotent (return existing session).
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier
	//
	// Returns:
	//   - string: Upload ID for this multipart upload session
	//   - error: Returns error if session cannot be initiated
	//
	// Example (COMMIT handler on first partial commit):
	//
	//	if incStore, ok := contentStore.(content.IncrementalWriteStore); ok {
	//	    uploadID, err := incStore.BeginIncrementalWrite(ctx, contentID)
	//	    if err != nil {
	//	        return fmt.Errorf("failed to begin incremental write: %w", err)
	//	    }
	//	    // Store uploadID in session state for subsequent commits
	//	}
	BeginIncrementalWrite(ctx context.Context, id metadata.ContentID) (uploadID string, err error)

	// FlushIncremental uploads complete parts from cache to content store.
	//
	// The implementation:
	//   1. Returns 0 if cacheSize < partSize (small file, nothing to upload yet)
	//   2. Calculates which complete parts can be uploaded: floor(cacheSize / partSize)
	//   3. Finds parts not yet uploaded and not currently uploading
	//   4. Uploads selected parts in parallel (up to maxParallelUploads)
	//   5. Updates cache.SetFlushedOffset() to highest contiguous uploaded position
	//
	// Part numbers are deterministic based on offset:
	//   partNumber = (offset / partSize) + 1
	//
	// This enables multiple concurrent COMMITs to upload different parts simultaneously
	// without coordination - each COMMIT calculates which parts it can upload.
	//
	// The implementation tracks per content ID:
	//   - uploadedParts: map of successfully uploaded part numbers
	//   - uploadingParts: map of parts currently being uploaded (prevents duplicates)
	//
	// Returns the number of bytes actually uploaded (0 if small file or all parts done).
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier
	//   - c: Cache to read data from
	//
	// Returns:
	//   - flushed: Number of bytes actually uploaded to storage
	//   - error: Returns error if read or upload fails
	//
	// Example (COMMIT handler):
	//
	//	if incStore, ok := contentStore.(content.IncrementalWriteStore); ok {
	//	    flushed, err := incStore.FlushIncremental(ctx, contentID, cache)
	//	    if err != nil {
	//	        return fmt.Errorf("failed to flush incremental: %w", err)
	//	    }
	//	    if flushed > 0 {
	//	        cache.SetState(contentID, StateUploading)
	//	    }
	//	}
	FlushIncremental(ctx context.Context, id metadata.ContentID, c cache.Cache) (flushed int64, err error)

	// CompleteIncrementalWrite finalizes an incremental write session.
	//
	// This handles two cases:
	//
	// Small files (cacheSize < partSize):
	//   - No multipart upload was started
	//   - Uses simple PutObject to upload directly from cache
	//   - Single API call (efficient)
	//
	// Large files (cacheSize >= partSize):
	//   - Uploads any remaining parts not yet uploaded (including final partial part)
	//   - Calls CompleteMultipartUpload with list of all part numbers
	//   - Cleans up session state
	//
	// After this call, the content is available for reading via ReadContent().
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier
	//   - c: Cache to read data from (for small files or remaining parts)
	//
	// Returns:
	//   - error: Returns error if completion fails
	//
	// Example (Background flusher):
	//
	//	if incStore, ok := contentStore.(content.IncrementalWriteStore); ok {
	//	    err := incStore.CompleteIncrementalWrite(ctx, contentID, cache)
	//	    if err != nil {
	//	        return fmt.Errorf("failed to complete incremental write: %w", err)
	//	    }
	//	    cache.SetState(contentID, StateCached)
	//	}
	CompleteIncrementalWrite(ctx context.Context, id metadata.ContentID, c cache.Cache) error

	// AbortIncrementalWrite cancels an incremental write session.
	//
	// This:
	//   1. Aborts the S3 multipart upload (frees storage)
	//   2. Discards any buffered data
	//   3. Cleans up session state
	//
	// This operation is idempotent - aborting a non-existent session succeeds.
	//
	// Use Cases:
	//   - WRITE or COMMIT operation failed
	//   - Client disconnected during upload
	//   - Timeout exceeded
	//   - Cleanup on server shutdown
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - id: Content identifier
	//
	// Returns:
	//   - error: Returns error only for storage failures (idempotent)
	//
	// Example (COMMIT handler on error):
	//
	//	defer func() {
	//	    if err != nil && incStore != nil {
	//	        incStore.AbortIncrementalWrite(context.Background(), contentID)
	//	    }
	//	}()
	AbortIncrementalWrite(ctx context.Context, id metadata.ContentID) error

	// GetIncrementalWriteState returns the current state of an incremental write session.
	//
	// This allows checking if an incremental write is in progress and getting
	// information about uploaded/uploading parts.
	//
	// Returns nil if no incremental write session exists for this content ID.
	//
	// Parameters:
	//   - id: Content identifier
	//
	// Returns:
	//   - *IncrementalWriteState: Current state (nil if no session)
	//
	// Example (Background flusher):
	//
	//	state := incStore.GetIncrementalWriteState(contentID)
	//	if state != nil && state.PartsUploading > 0 {
	//	    // Parts still being uploaded, wait before finalizing
	//	    continue
	//	}
	GetIncrementalWriteState(id metadata.ContentID) *IncrementalWriteState
}

// IncrementalWriteState tracks the state of an incremental write session.
type IncrementalWriteState struct {
	// UploadID is the S3 multipart upload ID (empty if not yet started)
	UploadID string

	// PartsWritten is the count of successfully uploaded parts
	PartsWritten int

	// PartsWriting is the count of parts currently being uploaded
	// Used by flusher to avoid finalizing while uploads in progress
	PartsWriting int

	// TotalFlushed is the total bytes uploaded so far
	TotalFlushed int64
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
