package blocks

import "errors"

// ============================================================================
// Standard Block Service Errors
// ============================================================================

// These errors provide a consistent way to indicate common failure conditions
// across block operations. Protocol handlers should check for these errors
// and map them to appropriate protocol-specific error codes.

var (
	// ErrContentNotFound indicates the requested content does not exist.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrNoEnt (2)
	//   - SMB: STATUS_OBJECT_NAME_NOT_FOUND
	//   - HTTP: 404 Not Found
	ErrContentNotFound = errors.New("content not found")

	// ErrContentExists indicates content with this ID already exists.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrExist (17)
	//   - SMB: STATUS_OBJECT_NAME_COLLISION
	//   - HTTP: 409 Conflict
	ErrContentExists = errors.New("content already exists")

	// ErrInvalidOffset indicates the offset is invalid for the operation.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrInval (22)
	//   - SMB: STATUS_INVALID_PARAMETER
	//   - HTTP: 416 Range Not Satisfiable
	ErrInvalidOffset = errors.New("invalid offset")

	// ErrInvalidSize indicates the size parameter is invalid.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrInval (22)
	//   - SMB: STATUS_INVALID_PARAMETER
	ErrInvalidSize = errors.New("invalid size")

	// ErrStorageFull indicates the storage backend has no available space.
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
	// Protocol Mapping:
	//   - NFS: NFS3ErrDQuot (69)
	//   - SMB: STATUS_QUOTA_EXCEEDED
	//   - HTTP: 507 Insufficient Storage
	ErrQuotaExceeded = errors.New("quota exceeded")

	// ErrIntegrityCheckFailed indicates content integrity verification failed.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrIO (5)
	//   - SMB: STATUS_DATA_CHECKSUM_ERROR
	//   - HTTP: 500 Internal Server Error
	ErrIntegrityCheckFailed = errors.New("integrity check failed")

	// ErrReadOnly indicates the content store is read-only.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrRoFs (30)
	//   - SMB: STATUS_MEDIA_WRITE_PROTECTED
	//   - HTTP: 403 Forbidden
	ErrReadOnly = errors.New("content store is read-only")

	// ErrNotSupported indicates the operation is not supported.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrNotSupp (10004)
	//   - SMB: STATUS_NOT_SUPPORTED
	//   - HTTP: 501 Not Implemented
	ErrNotSupported = errors.New("operation not supported")

	// ErrConcurrentModification indicates content was modified concurrently.
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
	// Protocol Mapping:
	//   - NFS: NFS3ErrBadHandle (10001)
	//   - SMB: STATUS_INVALID_PARAMETER
	//   - HTTP: 400 Bad Request
	ErrInvalidContentID = errors.New("invalid content ID")

	// ErrTooLarge indicates the content or operation is too large.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrFBig (27)
	//   - SMB: STATUS_FILE_TOO_LARGE
	//   - HTTP: 413 Payload Too Large
	ErrTooLarge = errors.New("content too large")

	// ErrUnavailable indicates the storage backend is temporarily unavailable.
	//
	// This is a transient error - retrying may succeed.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrJukebox (10008)
	//   - SMB: STATUS_DEVICE_NOT_READY
	//   - HTTP: 503 Service Unavailable
	ErrUnavailable = errors.New("storage unavailable")

	// ErrNoSliceCacheForShare indicates no slice cache is configured for the share.
	// Deprecated: Use ErrNoCacheConfigured instead.
	ErrNoSliceCacheForShare = errors.New("no slice cache configured for share")

	// ErrNoCacheConfigured indicates no cache is configured for the block service.
	ErrNoCacheConfigured = errors.New("no cache configured")
)
