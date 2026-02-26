package payload

import (
	"errors"
	"fmt"
	"time"
)

// Standard block service errors. Protocol handlers should check for these errors
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

	// ErrInvalidPayloadID indicates the PayloadID format is invalid.
	//
	// Protocol Mapping:
	//   - NFS: NFS3ErrBadHandle (10001)
	//   - SMB: STATUS_INVALID_PARAMETER
	//   - HTTP: 400 Bad Request
	ErrInvalidPayloadID = errors.New("invalid content ID")

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
)

// PayloadError wraps sentinel payload errors with structured debugging context.
//
// It provides rich operational metadata for diagnosing payload storage issues
// without losing compatibility with errors.Is() checks on the underlying sentinel.
// For example:
//
//	err := NewPayloadError("upload", "/archive", "abc123", 5, "s3", ErrUnavailable)
//	errors.Is(err, ErrUnavailable) // true
//
// Fields capture the operation type, affected share, payload identifier, block
// index, backend type, and the wrapped sentinel error. Optional fields (Size,
// Duration, Retries) can be set after construction for performance debugging.
type PayloadError struct {
	// Op describes the operation that failed: "upload", "download", "dedup", or "gc".
	Op string

	// Share is the share name providing routing context for the error.
	Share string

	// PayloadID is the content identifier of the affected payload.
	PayloadID string

	// BlockIdx is the block index within the chunk that failed.
	BlockIdx uint32

	// Size is the data size involved in the operation (bytes).
	Size int64

	// Duration is how long the operation ran before failing.
	Duration time.Duration

	// Retries is the number of retry attempts made before the final failure.
	Retries int

	// Backend identifies the storage backend type: "s3", "memory", or "filesystem".
	Backend string

	// Err is the wrapped sentinel error (e.g., ErrContentNotFound, ErrUnavailable).
	Err error
}

// Error returns a human-readable description of the payload error including
// the operation, underlying error, and key context fields.
func (e *PayloadError) Error() string {
	return fmt.Sprintf("payload %s: %s (share=%s, payload=%s, block=%d, backend=%s)",
		e.Op, e.Err, e.Share, e.PayloadID, e.BlockIdx, e.Backend)
}

// Unwrap returns the underlying sentinel error, enabling errors.Is() and
// errors.As() to match through PayloadError wrapping.
func (e *PayloadError) Unwrap() error {
	return e.Err
}

// NewPayloadError creates a PayloadError wrapping the given sentinel error
// with operational context. Optional fields (Size, Duration, Retries) default
// to zero and can be set on the returned pointer after construction.
func NewPayloadError(op, share, payloadID string, blockIdx uint32, backend string, err error) *PayloadError {
	return &PayloadError{
		Op:        op,
		Share:     share,
		PayloadID: payloadID,
		BlockIdx:  blockIdx,
		Backend:   backend,
		Err:       err,
	}
}
