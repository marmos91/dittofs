// Package errors provides error types and error codes for the metadata package.
// This is a leaf package with no internal dependencies, designed to be imported
// by both the lock package and metadata store implementations without causing
// circular imports.
//
// Import graph: errors <- lock <- metadata <- store implementations
package errors

import (
	"fmt"
)

// ErrorCode represents the type of error that occurred.
type ErrorCode int

const (
	// ErrNotFound indicates the requested resource does not exist.
	ErrNotFound ErrorCode = iota + 1

	// ErrAccessDenied indicates permission bit violations (POSIX EACCES).
	// Used when the caller lacks the required read/write/execute permission bits.
	ErrAccessDenied

	// ErrAuthRequired indicates authentication is required but not provided.
	ErrAuthRequired

	// ErrPermissionDenied indicates operation not permitted (POSIX EPERM).
	// Used when the operation requires ownership or root privileges.
	ErrPermissionDenied

	// ErrAlreadyExists indicates the resource already exists.
	ErrAlreadyExists

	// ErrNotEmpty indicates directory is not empty.
	ErrNotEmpty

	// ErrIsDirectory indicates operation not valid on directory.
	ErrIsDirectory

	// ErrNotDirectory indicates operation requires a directory.
	ErrNotDirectory

	// ErrInvalidArgument indicates an invalid argument was provided.
	ErrInvalidArgument

	// ErrIOError indicates an I/O error occurred.
	ErrIOError

	// ErrNoSpace indicates no space is available.
	ErrNoSpace

	// ErrQuotaExceeded indicates quota has been exceeded.
	ErrQuotaExceeded

	// ErrReadOnly indicates operation failed because filesystem is read-only.
	ErrReadOnly

	// ErrNotSupported indicates operation is not supported by implementation.
	ErrNotSupported

	// ErrInvalidHandle indicates the file handle is invalid.
	ErrInvalidHandle

	// ErrStaleHandle indicates the file handle is valid but stale.
	ErrStaleHandle

	// ErrLocked indicates the resource is locked.
	ErrLocked

	// ErrLockNotFound indicates the specified lock does not exist.
	ErrLockNotFound

	// ErrPrivilegeRequired indicates elevated privileges are required.
	ErrPrivilegeRequired

	// ErrNameTooLong indicates the name exceeds maximum length.
	ErrNameTooLong

	// ErrDeadlock indicates a deadlock would occur.
	ErrDeadlock

	// ErrGracePeriod indicates operation blocked by grace period.
	ErrGracePeriod

	// ErrLockLimitExceeded indicates lock limits have been exceeded.
	ErrLockLimitExceeded

	// ErrLockConflict indicates a lock conflict (enhanced lock types).
	ErrLockConflict

	// ErrConnectionLimitReached indicates connection limit has been reached.
	ErrConnectionLimitReached
)

// String returns a human-readable name for the error code.
func (e ErrorCode) String() string {
	switch e {
	case ErrNotFound:
		return "NotFound"
	case ErrAccessDenied:
		return "AccessDenied"
	case ErrAuthRequired:
		return "AuthRequired"
	case ErrPermissionDenied:
		return "PermissionDenied"
	case ErrAlreadyExists:
		return "AlreadyExists"
	case ErrNotEmpty:
		return "NotEmpty"
	case ErrIsDirectory:
		return "IsDirectory"
	case ErrNotDirectory:
		return "NotDirectory"
	case ErrInvalidArgument:
		return "InvalidArgument"
	case ErrIOError:
		return "IOError"
	case ErrNoSpace:
		return "NoSpace"
	case ErrQuotaExceeded:
		return "QuotaExceeded"
	case ErrReadOnly:
		return "ReadOnly"
	case ErrNotSupported:
		return "NotSupported"
	case ErrInvalidHandle:
		return "InvalidHandle"
	case ErrStaleHandle:
		return "StaleHandle"
	case ErrLocked:
		return "Locked"
	case ErrLockNotFound:
		return "LockNotFound"
	case ErrPrivilegeRequired:
		return "PrivilegeRequired"
	case ErrNameTooLong:
		return "NameTooLong"
	case ErrDeadlock:
		return "Deadlock"
	case ErrGracePeriod:
		return "GracePeriod"
	case ErrLockLimitExceeded:
		return "LockLimitExceeded"
	case ErrLockConflict:
		return "LockConflict"
	case ErrConnectionLimitReached:
		return "ConnectionLimitReached"
	default:
		return fmt.Sprintf("Unknown(%d)", e)
	}
}

// StoreError represents a metadata store error with an error code.
type StoreError struct {
	Code    ErrorCode
	Message string
	Path    string
}

// Error implements the error interface.
func (e *StoreError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s: %s (path: %s)", e.Code, e.Message, e.Path)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// ============================================================================
// Generic Factory Functions (no lock type dependencies)
// ============================================================================

// NewNotFoundError creates a NotFound error.
func NewNotFoundError(path, resourceType string) *StoreError {
	return &StoreError{
		Code:    ErrNotFound,
		Message: fmt.Sprintf("%s not found", resourceType),
		Path:    path,
	}
}

// NewPermissionDeniedError creates a PermissionDenied error.
func NewPermissionDeniedError(path string) *StoreError {
	return &StoreError{
		Code:    ErrPermissionDenied,
		Message: "permission denied",
		Path:    path,
	}
}

// NewIsDirectoryError creates an IsDirectory error.
func NewIsDirectoryError(path string) *StoreError {
	return &StoreError{
		Code:    ErrIsDirectory,
		Message: "is a directory",
		Path:    path,
	}
}

// NewNotDirectoryError creates a NotDirectory error.
func NewNotDirectoryError(path string) *StoreError {
	return &StoreError{
		Code:    ErrNotDirectory,
		Message: "not a directory",
		Path:    path,
	}
}

// NewInvalidHandleError creates an InvalidHandle error.
func NewInvalidHandleError() *StoreError {
	return &StoreError{
		Code:    ErrInvalidHandle,
		Message: "invalid file handle",
	}
}

// NewNotEmptyError creates a NotEmpty error.
func NewNotEmptyError(path string) *StoreError {
	return &StoreError{
		Code:    ErrNotEmpty,
		Message: "directory not empty",
		Path:    path,
	}
}

// NewAlreadyExistsError creates an AlreadyExists error.
func NewAlreadyExistsError(path string) *StoreError {
	return &StoreError{
		Code:    ErrAlreadyExists,
		Message: "already exists",
		Path:    path,
	}
}

// NewInvalidArgumentError creates an InvalidArgument error.
func NewInvalidArgumentError(message string) *StoreError {
	return &StoreError{
		Code:    ErrInvalidArgument,
		Message: message,
	}
}

// NewAccessDeniedError creates an AccessDenied error.
func NewAccessDeniedError(reason string) *StoreError {
	return &StoreError{
		Code:    ErrAccessDenied,
		Message: reason,
	}
}

// NewQuotaExceededError creates a QuotaExceeded error.
func NewQuotaExceededError(path string) *StoreError {
	return &StoreError{
		Code:    ErrQuotaExceeded,
		Message: "disk quota exceeded",
		Path:    path,
	}
}

// NewPrivilegeRequiredError creates a PrivilegeRequired error.
func NewPrivilegeRequiredError(operation string) *StoreError {
	return &StoreError{
		Code:    ErrPrivilegeRequired,
		Message: fmt.Sprintf("operation requires root privileges: %s", operation),
	}
}

// NewNameTooLongError creates a NameTooLong error.
func NewNameTooLongError(path string) *StoreError {
	return &StoreError{
		Code:    ErrNameTooLong,
		Message: "name too long",
		Path:    path,
	}
}

// NewConnectionLimitError creates a connection limit exceeded error.
func NewConnectionLimitError(adapterType string, limit int) *StoreError {
	return &StoreError{
		Code:    ErrConnectionLimitReached,
		Message: fmt.Sprintf("connection limit reached for %s adapter (max: %d)", adapterType, limit),
	}
}

// ============================================================================
// Error Type Checking Helpers
// ============================================================================

// IsNotFoundError returns true if the error is a NotFound error.
func IsNotFoundError(err error) bool {
	if storeErr, ok := err.(*StoreError); ok {
		return storeErr.Code == ErrNotFound || storeErr.Code == ErrLockNotFound
	}
	return false
}

// IsLockConflictError returns true if the error is a lock conflict.
func IsLockConflictError(err error) bool {
	if storeErr, ok := err.(*StoreError); ok {
		return storeErr.Code == ErrLocked || storeErr.Code == ErrLockConflict
	}
	return false
}

// IsDeadlockError returns true if the error indicates a deadlock.
func IsDeadlockError(err error) bool {
	if storeErr, ok := err.(*StoreError); ok {
		return storeErr.Code == ErrDeadlock
	}
	return false
}

// IsGracePeriodError returns true if the error is due to grace period.
func IsGracePeriodError(err error) bool {
	if storeErr, ok := err.(*StoreError); ok {
		return storeErr.Code == ErrGracePeriod
	}
	return false
}

// IsLockLimitError returns true if the error is due to lock limits.
func IsLockLimitError(err error) bool {
	if storeErr, ok := err.(*StoreError); ok {
		return storeErr.Code == ErrLockLimitExceeded
	}
	return false
}
