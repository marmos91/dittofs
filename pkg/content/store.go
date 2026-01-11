package content

import (
	"context"
	"io"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// ContentStore Interface
// ============================================================================

// ContentStore provides protocol-agnostic content management for file data storage.
//
// This interface is designed to abstract away the underlying storage mechanism
// (filesystem, S3, memory, etc.) and provide a consistent API for file content
// operations. Protocol handlers interact with metadata via MetadataService and
// file content via ContentService.
//
// Separation of Concerns:
//
// The content store manages only the raw file data (bytes). It does NOT manage:
//   - File metadata (attributes, permissions, ownership) -> handled by MetadataService
//   - File hierarchy and directory structure -> handled by MetadataService
//   - Access control and permissions -> handled by MetadataService
//   - File handles and path resolution -> handled by MetadataService
//
// Content Coordination:
// The MetadataService and ContentService are designed to work together:
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
// read and write operations. Optional interfaces (ReadAtContentStore,
// IncrementalWriteStore) extend this base interface with additional capabilities.
type ContentStore interface {
	// ========================================================================
	// Content Reading
	// ========================================================================

	// ReadContent returns a reader for the content identified by the given ID.
	//
	// The returned reader provides sequential access to the content data.
	// The caller is responsible for closing the reader when done.
	//
	// For random access reads, use ReadAtContentStore.ReadAt() if available.
	//
	// Context Cancellation:
	// The method checks context before opening content. Once the reader is
	// returned, callers should monitor context and close the reader if
	// cancelled.
	//
	// Returns:
	//   - io.ReadCloser: Reader for the content (must be closed by caller)
	//   - error: ErrContentNotFound if content doesn't exist, or context/IO errors
	ReadContent(ctx context.Context, id metadata.ContentID) (io.ReadCloser, error)

	// GetContentSize returns the size of the content in bytes.
	//
	// This is a lightweight operation that returns content size without reading
	// the data. Useful for NFS GETATTR operations and buffer allocation.
	//
	// Returns:
	//   - uint64: Size of the content in bytes
	//   - error: ErrContentNotFound if content doesn't exist, or context/IO errors
	GetContentSize(ctx context.Context, id metadata.ContentID) (uint64, error)

	// ContentExists checks if content with the given ID exists.
	//
	// This is a lightweight existence check that doesn't read content data
	// or metadata. Useful for garbage collection and validation.
	//
	// Returns:
	//   - bool: True if content exists, false otherwise
	//   - error: Only returns error for context cancellation or storage failures
	ContentExists(ctx context.Context, id metadata.ContentID) (bool, error)

	// ========================================================================
	// Storage Information
	// ========================================================================

	// GetStorageStats returns statistics about the content storage.
	//
	// For some backends (S3, distributed storage), gathering stats may be
	// expensive. Implementations should consider caching stats with TTL.
	//
	// Returns:
	//   - *StorageStats: Current storage statistics
	//   - error: Only context cancellation or storage access errors
	GetStorageStats(ctx context.Context) (*StorageStats, error)

	// ========================================================================
	// Health Check
	// ========================================================================

	// Healthcheck performs a lightweight health check on the content store.
	//
	// The check should be quick (ideally <1s) to avoid blocking health probes.
	//
	// Returns:
	//   - error: Returns error if store is unhealthy, nil if healthy
	Healthcheck(ctx context.Context) error

	// ========================================================================
	// Content Writing
	// ========================================================================

	// WriteAt writes data at the specified offset.
	//
	// This implements partial file updates for NFS WRITE operations. The content
	// will be created if it doesn't exist. If the offset is beyond the current
	// content size, the gap is filled with zeros (sparse file behavior).
	//
	// Returns:
	//   - error: Returns error if write fails or context is cancelled
	WriteAt(ctx context.Context, id metadata.ContentID, data []byte, offset uint64) error

	// Truncate changes the size of the content.
	//
	// This implements file size changes for NFS SETATTR operations:
	//   - If newSize < currentSize: Content is truncated (data removed)
	//   - If newSize > currentSize: Content is extended (zeros added)
	//   - If newSize == currentSize: No-op (succeeds immediately)
	//
	// Returns:
	//   - error: ErrContentNotFound if content doesn't exist, or context/IO errors
	Truncate(ctx context.Context, id metadata.ContentID, newSize uint64) error

	// Delete removes content from the store.
	//
	// The operation is idempotent - deleting non-existent content returns nil.
	//
	// Returns:
	//   - error: Only returns error for context cancellation or storage failures
	Delete(ctx context.Context, id metadata.ContentID) error

	// WriteContent writes the entire content in one operation.
	//
	// If content with this ID already exists, it is overwritten (replaced).
	//
	// Returns:
	//   - error: Returns error if write fails or context is cancelled
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
// Implementations:
//   - S3: MUST implement for acceptable performance
//   - Filesystem: Should implement for efficiency
//   - Memory: Optional (less critical for in-memory storage)
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
	// For S3 backends, this uses HTTP Range requests which is dramatically
	// more efficient than ReadContent() for partial reads.
	//
	// Returns:
	//   - n: Number of bytes read (may be less than len(p) on error or EOF)
	//   - error: io.EOF if offset is at/past end, or other read errors
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
//  1. FlushIncremental(contentID, cache) -> uploads complete parts in parallel
//  2. CompleteIncrementalWrite(contentID, cache) -> finalizes the upload
//
// Implementations:
//   - S3: MUST implement using native multipart uploads
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
	// Returns:
	//   - string: Upload ID for this multipart upload session
	//   - error: Returns error if session cannot be initiated
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
	// Returns the number of bytes actually uploaded (0 if small file or all parts done).
	//
	// Returns:
	//   - flushed: Number of bytes actually uploaded to storage
	//   - error: Returns error if read or upload fails
	FlushIncremental(ctx context.Context, id metadata.ContentID, c cache.Cache) (flushed uint64, err error)

	// CompleteIncrementalWrite finalizes an incremental write session.
	//
	// This handles two cases:
	//
	// Small files (cacheSize < partSize):
	//   - No multipart upload was started
	//   - Uses simple PutObject to upload directly from cache
	//
	// Large files (cacheSize >= partSize):
	//   - Uploads any remaining parts not yet uploaded (including final partial part)
	//   - Calls CompleteMultipartUpload with list of all part numbers
	//   - Cleans up session state
	//
	// After this call, the content is available for reading via ReadContent().
	//
	// Returns:
	//   - error: Returns error if completion fails
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
	// Returns:
	//   - error: Returns error only for storage failures (idempotent)
	AbortIncrementalWrite(ctx context.Context, id metadata.ContentID) error

	// GetIncrementalWriteState returns the current state of an incremental write session.
	//
	// Returns nil if no incremental write session exists for this content ID.
	//
	// Returns:
	//   - *IncrementalWriteState: Current state (nil if no session)
	GetIncrementalWriteState(id metadata.ContentID) *IncrementalWriteState
}
