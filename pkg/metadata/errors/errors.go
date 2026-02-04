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

	// ErrPermissionDenied indicates insufficient permissions.
	ErrPermissionDenied

	// ErrIsDirectory indicates operation not valid on directory.
	ErrIsDirectory

	// ErrNotDirectory indicates operation requires a directory.
	ErrNotDirectory

	// ErrInvalidHandle indicates the file handle is invalid.
	ErrInvalidHandle

	// ErrNotEmpty indicates directory is not empty.
	ErrNotEmpty

	// ErrAlreadyExists indicates the resource already exists.
	ErrAlreadyExists

	// ErrInvalidArgument indicates an invalid argument was provided.
	ErrInvalidArgument

	// ErrAccessDenied indicates access was denied.
	ErrAccessDenied

	// ErrQuotaExceeded indicates quota has been exceeded.
	ErrQuotaExceeded

	// ErrPrivilegeRequired indicates elevated privileges are required.
	ErrPrivilegeRequired

	// ErrNameTooLong indicates the name exceeds maximum length.
	ErrNameTooLong

	// ErrLocked indicates the resource is locked.
	ErrLocked

	// ErrLockNotFound indicates the specified lock does not exist.
	ErrLockNotFound

	// ErrLockConflict indicates a lock conflict (enhanced lock types).
	ErrLockConflict

	// ErrDeadlock indicates a deadlock would occur.
	ErrDeadlock

	// ErrGracePeriod indicates operation blocked by grace period.
	ErrGracePeriod

	// ErrLockLimitExceeded indicates lock limits have been exceeded.
	ErrLockLimitExceeded

	// ErrConnectionLimitReached indicates connection limit has been reached.
	ErrConnectionLimitReached
)

// String returns a human-readable name for the error code.
func (e ErrorCode) String() string {
	switch e {
	case ErrNotFound:
		return "NotFound"
	case ErrPermissionDenied:
		return "PermissionDenied"
	case ErrIsDirectory:
		return "IsDirectory"
	case ErrNotDirectory:
		return "NotDirectory"
	case ErrInvalidHandle:
		return "InvalidHandle"
	case ErrNotEmpty:
		return "NotEmpty"
	case ErrAlreadyExists:
		return "AlreadyExists"
	case ErrInvalidArgument:
		return "InvalidArgument"
	case ErrAccessDenied:
		return "AccessDenied"
	case ErrQuotaExceeded:
		return "QuotaExceeded"
	case ErrPrivilegeRequired:
		return "PrivilegeRequired"
	case ErrNameTooLong:
		return "NameTooLong"
	case ErrLocked:
		return "Locked"
	case ErrLockNotFound:
		return "LockNotFound"
	case ErrLockConflict:
		return "LockConflict"
	case ErrDeadlock:
		return "Deadlock"
	case ErrGracePeriod:
		return "GracePeriod"
	case ErrLockLimitExceeded:
		return "LockLimitExceeded"
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
func NewPermissionDeniedError(path, operation string) *StoreError {
	return &StoreError{
		Code:    ErrPermissionDenied,
		Message: fmt.Sprintf("permission denied for %s", operation),
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
func NewInvalidHandleError(handle string) *StoreError {
	return &StoreError{
		Code:    ErrInvalidHandle,
		Message: "invalid file handle",
		Path:    handle,
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
func NewAccessDeniedError(path, reason string) *StoreError {
	return &StoreError{
		Code:    ErrAccessDenied,
		Message: reason,
		Path:    path,
	}
}

// NewQuotaExceededError creates a QuotaExceeded error.
func NewQuotaExceededError(path string) *StoreError {
	return &StoreError{
		Code:    ErrQuotaExceeded,
		Message: "quota exceeded",
		Path:    path,
	}
}

// NewPrivilegeRequiredError creates a PrivilegeRequired error.
func NewPrivilegeRequiredError(operation string) *StoreError {
	return &StoreError{
		Code:    ErrPrivilegeRequired,
		Message: fmt.Sprintf("elevated privileges required for %s", operation),
	}
}

// NewNameTooLongError creates a NameTooLong error.
func NewNameTooLongError(name string, maxLen int) *StoreError {
	return &StoreError{
		Code:    ErrNameTooLong,
		Message: fmt.Sprintf("name exceeds maximum length of %d", maxLen),
		Path:    name,
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
