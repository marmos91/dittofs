package content

import "errors"

// ============================================================================
// Standard Content Store Errors
// ============================================================================

// These errors provide a consistent way to indicate common failure conditions
// across all content store implementations. Protocol handlers should check for
// these errors and map them to appropriate protocol-specific error codes.
//
// Usage Pattern:
//
//	content, err := store.ReadContent(ctx, id)
//	if err != nil {
//	    if errors.Is(err, content.ErrContentNotFound) {
//	        return nfs.NFS3ErrNoEnt
//	    }
//	    return nfs.NFS3ErrIO
//	}
//
// Error Wrapping:
// Implementations should wrap these errors with additional context:
//
//	if !fileExists {
//	    return fmt.Errorf("content %s: %w", id, content.ErrContentNotFound)
//	}

var (
	// ErrContentNotFound indicates the requested content does not exist.
	//
	// This error is returned when:
	//   - ReadContent() called with non-existent ContentID
	//   - GetContentSize() called with non-existent ContentID
	//   - Truncate() called with non-existent ContentID
	//   - Operations that require existing content
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrNoEnt (2)
	//   - SMB: STATUS_OBJECT_NAME_NOT_FOUND
	//   - HTTP: 404 Not Found
	ErrContentNotFound = errors.New("content not found")

	// ErrContentExists indicates content with this ID already exists.
	//
	// This error is returned when:
	//   - Creating new content with explicit "must not exist" semantics
	//   - Exclusive create operations
	//
	// Note: Most write operations overwrite existing content and do NOT
	// return this error. This is only for explicit "create new" operations.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrExist (17)
	//   - SMB: STATUS_OBJECT_NAME_COLLISION
	//   - HTTP: 409 Conflict
	ErrContentExists = errors.New("content already exists")

	// ErrInvalidOffset indicates the offset is invalid for the operation.
	//
	// This error is returned when:
	//   - Offset is negative
	//   - Offset would cause integer overflow
	//   - Offset exceeds implementation limits
	//
	// Note: Offset beyond current size is typically NOT an error for WriteAt
	// (sparse file semantics apply). This is for truly invalid offsets.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrInval (22)
	//   - SMB: STATUS_INVALID_PARAMETER
	//   - HTTP: 416 Range Not Satisfiable
	ErrInvalidOffset = errors.New("invalid offset")

	// ErrInvalidSize indicates the size parameter is invalid.
	//
	// This error is returned when:
	//   - Size is negative (if using signed integers)
	//   - Size would cause integer overflow
	//   - Size exceeds implementation limits
	//   - Size is invalid for the operation
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrInval (22)
	//   - SMB: STATUS_INVALID_PARAMETER
	ErrInvalidSize = errors.New("invalid size")

	// ErrStorageFull indicates the storage backend has no available space.
	//
	// This error is returned when:
	//   - Filesystem is full (disk space exhausted)
	//   - Storage quota is reached
	//   - No more storage can be allocated
	//
	// This is a transient error - it may succeed after cleanup.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrNoSpc (28)
	//   - SMB: STATUS_DISK_FULL
	//   - HTTP: 507 Insufficient Storage
	ErrStorageFull = errors.New("storage full")

	// ErrQuotaExceeded indicates a storage quota has been exceeded.
	//
	// This error is returned when:
	//   - User quota exceeded
	//   - Group quota exceeded
	//   - Project quota exceeded
	//   - Per-content limits exceeded
	//
	// Similar to ErrStorageFull but specifically for quota enforcement.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrDQuot (69)
	//   - SMB: STATUS_QUOTA_EXCEEDED
	//   - HTTP: 507 Insufficient Storage
	ErrQuotaExceeded = errors.New("quota exceeded")

	// ErrIntegrityCheckFailed indicates content integrity verification failed.
	//
	// This error is returned when:
	//   - Checksum mismatch detected
	//   - Hash verification failed
	//   - Content corruption detected
	//   - Cryptographic signature invalid
	//
	// This indicates data corruption or tampering.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrIO (5)
	//   - SMB: STATUS_DATA_CHECKSUM_ERROR
	//   - HTTP: 500 Internal Server Error
	ErrIntegrityCheckFailed = errors.New("integrity check failed")

	// ErrReadOnly indicates the content store is read-only.
	//
	// This error is returned when:
	//   - Attempting write operations on read-only store
	//   - Storage is mounted read-only
	//   - Storage is in maintenance mode
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrRoFs (30)
	//   - SMB: STATUS_MEDIA_WRITE_PROTECTED
	//   - HTTP: 403 Forbidden
	ErrReadOnly = errors.New("content store is read-only")

	// ErrNotSupported indicates the operation is not supported.
	//
	// This error is returned when:
	//   - Backend doesn't implement optional interface
	//   - Operation not supported by storage type
	//   - Feature not enabled
	//
	// This is a permanent error - retrying won't help.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrNotSupp (10004)
	//   - SMB: STATUS_NOT_SUPPORTED
	//   - HTTP: 501 Not Implemented
	ErrNotSupported = errors.New("operation not supported")

	// ErrConcurrentModification indicates content was modified concurrently.
	//
	// This error is returned when:
	//   - Optimistic locking detected conflict
	//   - ETag/version mismatch
	//   - Concurrent write detected
	//
	// Callers should retry with fresh data.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrStale (70) or NFS3ErrJukebox (10008)
	//   - SMB: STATUS_FILE_LOCK_CONFLICT
	//   - HTTP: 409 Conflict or 412 Precondition Failed
	ErrConcurrentModification = errors.New("concurrent modification detected")

	// ErrInvalidContentID indicates the ContentID format is invalid.
	//
	// This error is returned when:
	//   - ContentID contains invalid characters
	//   - ContentID format doesn't match expected pattern
	//   - ContentID is malformed
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrBadHandle (10001)
	//   - SMB: STATUS_INVALID_PARAMETER
	//   - HTTP: 400 Bad Request
	ErrInvalidContentID = errors.New("invalid content ID")

	// ErrTooLarge indicates the content or operation is too large.
	//
	// This error is returned when:
	//   - Content size exceeds maximum supported size
	//   - Write size exceeds maximum write size
	//   - Operation would exceed implementation limits
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrFBig (27)
	//   - SMB: STATUS_FILE_TOO_LARGE
	//   - HTTP: 413 Payload Too Large
	ErrTooLarge = errors.New("content too large")

	// ErrUnavailable indicates the storage backend is temporarily unavailable.
	//
	// This error is returned when:
	//   - Storage backend is offline
	//   - Network connection to storage failed
	//   - Storage is in maintenance mode
	//   - Too many concurrent requests
	//
	// This is a transient error - retrying may succeed.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrJukebox (10008)
	//   - SMB: STATUS_DEVICE_NOT_READY
	//   - HTTP: 503 Service Unavailable
	ErrUnavailable = errors.New("storage unavailable")
)
