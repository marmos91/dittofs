package metadata

import "fmt"

// StoreError represents a domain error from repository operations.
//
// These are business logic errors (file not found, permission denied, etc.)
// as opposed to infrastructure errors (network failure, disk error).
//
// Protocol handlers translate StoreError codes to protocol-specific
// error codes (e.g., NFS status codes, SMB error codes).
type StoreError struct {
	// Code is the error category
	Code ErrorCode

	// Message is a human-readable error description
	Message string

	// Path is the filesystem path related to the error (if applicable)
	Path string
}

// Error implements the error interface.
func (e *StoreError) Error() string {
	if e.Path != "" {
		return e.Message + ": " + e.Path
	}
	return e.Message
}

// ErrorCode represents the category of a repository error.
type ErrorCode int

const (
	// ErrNotFound indicates the requested file/directory/share doesn't exist
	ErrNotFound ErrorCode = iota

	// ErrAccessDenied indicates permission bit violations (POSIX EACCES).
	// Used when the caller lacks the required read/write/execute permission bits.
	// Maps to NFS3ErrAccess (EACCES).
	ErrAccessDenied

	// ErrAuthRequired indicates authentication is required but not provided
	ErrAuthRequired

	// ErrPermissionDenied indicates operation not permitted (POSIX EPERM).
	// Used when the operation requires ownership or root privileges.
	// Examples: chmod by non-owner, chown by non-root.
	// Maps to NFS3ErrPerm (EPERM).
	ErrPermissionDenied

	// ErrAlreadyExists indicates a file/directory with the name already exists
	ErrAlreadyExists

	// ErrNotEmpty indicates a directory is not empty (cannot be removed)
	ErrNotEmpty

	// ErrIsDirectory indicates operation expected a file but got a directory
	ErrIsDirectory

	// ErrNotDirectory indicates operation expected a directory but got a file
	ErrNotDirectory

	// ErrInvalidArgument indicates invalid parameters were provided
	ErrInvalidArgument

	// ErrIOError indicates an I/O error occurred
	ErrIOError

	// ErrNoSpace indicates no space is available
	ErrNoSpace

	// ErrQuotaExceeded indicates user's disk quota is exceeded
	ErrQuotaExceeded

	// ErrReadOnly indicates operation failed because filesystem is read-only
	ErrReadOnly

	// ErrNotSupported indicates operation is not supported by implementation
	ErrNotSupported

	// ErrInvalidHandle indicates the file handle is malformed
	ErrInvalidHandle

	// ErrStaleHandle indicates the file handle is valid but stale
	ErrStaleHandle

	// ErrLocked indicates a lock conflict exists
	ErrLocked

	// ErrLockNotFound indicates the requested lock doesn't exist
	ErrLockNotFound

	// ErrPrivilegeRequired indicates the operation requires elevated privileges
	ErrPrivilegeRequired

	// ErrNameTooLong indicates the path or filename exceeds system limits
	ErrNameTooLong
)

// ============================================================================
// Error Factory Functions
// ============================================================================

// NewNotFoundError creates a StoreError for when a file, directory, or share is not found.
func NewNotFoundError(path string, entityType string) *StoreError {
	return &StoreError{
		Code:    ErrNotFound,
		Message: entityType + " not found",
		Path:    path,
	}
}

// NewPermissionDeniedError creates a StoreError for permission denied errors.
func NewPermissionDeniedError(path string) *StoreError {
	return &StoreError{
		Code:    ErrPermissionDenied,
		Message: "permission denied",
		Path:    path,
	}
}

// NewIsDirectoryError creates a StoreError for when a file operation is attempted on a directory.
func NewIsDirectoryError(path string) *StoreError {
	return &StoreError{
		Code:    ErrIsDirectory,
		Message: "is a directory",
		Path:    path,
	}
}

// NewNotDirectoryError creates a StoreError for when a directory operation is attempted on a non-directory.
func NewNotDirectoryError(path string) *StoreError {
	return &StoreError{
		Code:    ErrNotDirectory,
		Message: "not a directory",
		Path:    path,
	}
}

// NewInvalidHandleError creates a StoreError for invalid file handles.
func NewInvalidHandleError() *StoreError {
	return &StoreError{
		Code:    ErrInvalidHandle,
		Message: "invalid file handle",
	}
}

// NewNotEmptyError creates a StoreError for when a directory is not empty.
func NewNotEmptyError(path string) *StoreError {
	return &StoreError{
		Code:    ErrNotEmpty,
		Message: "directory not empty",
		Path:    path,
	}
}

// NewAlreadyExistsError creates a StoreError for when a file/directory already exists.
func NewAlreadyExistsError(path string) *StoreError {
	return &StoreError{
		Code:    ErrAlreadyExists,
		Message: "already exists",
		Path:    path,
	}
}

// NewInvalidArgumentError creates a StoreError for invalid arguments.
func NewInvalidArgumentError(message string) *StoreError {
	return &StoreError{
		Code:    ErrInvalidArgument,
		Message: message,
	}
}

// NewAccessDeniedError creates a StoreError for share-level access denial.
func NewAccessDeniedError(reason string) *StoreError {
	return &StoreError{
		Code:    ErrAccessDenied,
		Message: reason,
	}
}

// NewLockedError creates a StoreError for lock conflicts.
func NewLockedError(path string, conflict *LockConflict) *StoreError {
	var msg string
	if conflict != nil {
		msg = fmt.Sprintf("file is locked by session %d (offset=%d, length=%d, exclusive=%v)",
			conflict.OwnerSessionID, conflict.Offset, conflict.Length, conflict.Exclusive)
	} else {
		msg = "file is locked"
	}
	return &StoreError{
		Code:    ErrLocked,
		Message: msg,
		Path:    path,
	}
}

// NewLockNotFoundError creates a StoreError for unlock operations on non-existent locks.
func NewLockNotFoundError(path string) *StoreError {
	return &StoreError{
		Code:    ErrLockNotFound,
		Message: "lock not found",
		Path:    path,
	}
}

// NewQuotaExceededError creates a StoreError for quota exceeded errors.
func NewQuotaExceededError(path string) *StoreError {
	return &StoreError{
		Code:    ErrQuotaExceeded,
		Message: "disk quota exceeded",
		Path:    path,
	}
}

// NewPrivilegeRequiredError creates a StoreError for operations requiring root.
func NewPrivilegeRequiredError(operation string) *StoreError {
	return &StoreError{
		Code:    ErrPrivilegeRequired,
		Message: fmt.Sprintf("operation requires root privileges: %s", operation),
	}
}

// NewNameTooLongError creates a StoreError for paths/names exceeding limits.
func NewNameTooLongError(path string) *StoreError {
	return &StoreError{
		Code:    ErrNameTooLong,
		Message: "name too long",
		Path:    path,
	}
}

// IsNotFoundError checks if an error is a StoreError with ErrNotFound code.
func IsNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	if storeErr, ok := err.(*StoreError); ok {
		return storeErr.Code == ErrNotFound
	}
	return false
}
