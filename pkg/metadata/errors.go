package metadata

import (
	"github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// Re-exported types from errors package for backward compatibility
// ============================================================================

// StoreError is re-exported from the errors package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/errors directly.
type StoreError = errors.StoreError

// ErrorCode is re-exported from the errors package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/errors directly.
type ErrorCode = errors.ErrorCode

// Re-exported error codes for backward compatibility.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/errors directly.
const (
	ErrNotFound               = errors.ErrNotFound
	ErrAccessDenied           = errors.ErrAccessDenied
	ErrAuthRequired           = errors.ErrAuthRequired
	ErrPermissionDenied       = errors.ErrPermissionDenied
	ErrAlreadyExists          = errors.ErrAlreadyExists
	ErrNotEmpty               = errors.ErrNotEmpty
	ErrIsDirectory            = errors.ErrIsDirectory
	ErrNotDirectory           = errors.ErrNotDirectory
	ErrInvalidArgument        = errors.ErrInvalidArgument
	ErrIOError                = errors.ErrIOError
	ErrNoSpace                = errors.ErrNoSpace
	ErrQuotaExceeded          = errors.ErrQuotaExceeded
	ErrReadOnly               = errors.ErrReadOnly
	ErrNotSupported           = errors.ErrNotSupported
	ErrInvalidHandle          = errors.ErrInvalidHandle
	ErrStaleHandle            = errors.ErrStaleHandle
	ErrLocked                 = errors.ErrLocked
	ErrLockNotFound           = errors.ErrLockNotFound
	ErrPrivilegeRequired      = errors.ErrPrivilegeRequired
	ErrNameTooLong            = errors.ErrNameTooLong
	ErrDeadlock               = errors.ErrDeadlock
	ErrGracePeriod            = errors.ErrGracePeriod
	ErrLockLimitExceeded      = errors.ErrLockLimitExceeded
	ErrLockConflict           = errors.ErrLockConflict
	ErrConnectionLimitReached = errors.ErrConnectionLimitReached
)

// ============================================================================
// Re-exported types from lock package for backward compatibility
// ============================================================================

// LockConflict is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type LockConflict = lock.LockConflict

// UnifiedLockConflict is re-exported from the lock package.
// Deprecated: Import from github.com/marmos91/dittofs/pkg/metadata/lock directly.
type UnifiedLockConflict = lock.UnifiedLockConflict

// ============================================================================
// Error Factory Functions (backward compatibility wrappers)
// ============================================================================

// NewNotFoundError creates a StoreError for when a file, directory, or share is not found.
// Deprecated: Use errors.NewNotFoundError directly.
func NewNotFoundError(path string, entityType string) *StoreError {
	return errors.NewNotFoundError(path, entityType)
}

// NewPermissionDeniedError creates a StoreError for permission denied errors.
// Deprecated: Use errors.NewPermissionDeniedError directly.
func NewPermissionDeniedError(path string) *StoreError {
	return errors.NewPermissionDeniedError(path)
}

// NewIsDirectoryError creates a StoreError for when a file operation is attempted on a directory.
// Deprecated: Use errors.NewIsDirectoryError directly.
func NewIsDirectoryError(path string) *StoreError {
	return errors.NewIsDirectoryError(path)
}

// NewNotDirectoryError creates a StoreError for when a directory operation is attempted on a non-directory.
// Deprecated: Use errors.NewNotDirectoryError directly.
func NewNotDirectoryError(path string) *StoreError {
	return errors.NewNotDirectoryError(path)
}

// NewInvalidHandleError creates a StoreError for invalid file handles.
// Deprecated: Use errors.NewInvalidHandleError directly.
func NewInvalidHandleError() *StoreError {
	return errors.NewInvalidHandleError()
}

// NewNotEmptyError creates a StoreError for when a directory is not empty.
// Deprecated: Use errors.NewNotEmptyError directly.
func NewNotEmptyError(path string) *StoreError {
	return errors.NewNotEmptyError(path)
}

// NewAlreadyExistsError creates a StoreError for when a file/directory already exists.
// Deprecated: Use errors.NewAlreadyExistsError directly.
func NewAlreadyExistsError(path string) *StoreError {
	return errors.NewAlreadyExistsError(path)
}

// NewInvalidArgumentError creates a StoreError for invalid arguments.
// Deprecated: Use errors.NewInvalidArgumentError directly.
func NewInvalidArgumentError(message string) *StoreError {
	return errors.NewInvalidArgumentError(message)
}

// NewAccessDeniedError creates a StoreError for share-level access denial.
// Deprecated: Use errors.NewAccessDeniedError directly.
func NewAccessDeniedError(reason string) *StoreError {
	return errors.NewAccessDeniedError(reason)
}

// NewLockedError creates a StoreError for lock conflicts.
// Deprecated: Use lock.NewLockedError directly.
func NewLockedError(path string, conflict *LockConflict) *StoreError {
	return lock.NewLockedError(path, conflict)
}

// NewLockNotFoundError creates a StoreError for unlock operations on non-existent locks.
// Deprecated: Use lock.NewLockNotFoundError directly.
func NewLockNotFoundError(path string) *StoreError {
	return lock.NewLockNotFoundError(path)
}

// NewQuotaExceededError creates a StoreError for quota exceeded errors.
// Deprecated: Use errors.NewQuotaExceededError directly.
func NewQuotaExceededError(path string) *StoreError {
	return errors.NewQuotaExceededError(path)
}

// NewPrivilegeRequiredError creates a StoreError for operations requiring root.
// Deprecated: Use errors.NewPrivilegeRequiredError directly.
func NewPrivilegeRequiredError(operation string) *StoreError {
	return errors.NewPrivilegeRequiredError(operation)
}

// NewNameTooLongError creates a StoreError for paths/names exceeding limits.
// Deprecated: Use errors.NewNameTooLongError directly.
func NewNameTooLongError(path string) *StoreError {
	return errors.NewNameTooLongError(path)
}

// NewDeadlockError creates a StoreError for deadlock detection.
// Deprecated: Use lock.NewDeadlockError directly.
func NewDeadlockError(waiter string, blockedBy []string) *StoreError {
	return lock.NewDeadlockError(waiter, blockedBy)
}

// NewGracePeriodError creates a StoreError for grace period blocking.
// Deprecated: Use lock.NewGracePeriodError directly.
func NewGracePeriodError(remainingSeconds int) *StoreError {
	return lock.NewGracePeriodError(remainingSeconds)
}

// NewLockLimitExceededError creates a StoreError for lock limit violations.
// Deprecated: Use lock.NewLockLimitExceededError directly.
func NewLockLimitExceededError(limitType string, current, max int) *StoreError {
	return lock.NewLockLimitExceededError(limitType, current, max)
}

// NewLockConflictError creates a StoreError for lock conflicts (upgrade, etc.).
// Deprecated: Use lock.NewLockConflictError directly.
func NewLockConflictError(path string, conflict *UnifiedLockConflict) *StoreError {
	return lock.NewLockConflictError(path, conflict)
}

// ============================================================================
// Error Helper Functions (backward compatibility wrappers)
// ============================================================================

// IsNotFoundError checks if an error is a StoreError with ErrNotFound code.
// Deprecated: Use errors.IsNotFoundError directly.
func IsNotFoundError(err error) bool {
	return errors.IsNotFoundError(err)
}

// IsLockConflictError checks if an error is a StoreError with ErrLockConflict code.
// Deprecated: Use errors.IsLockConflictError directly.
func IsLockConflictError(err error) bool {
	return errors.IsLockConflictError(err)
}

// IsDeadlockError checks if an error is a StoreError with ErrDeadlock code.
// Deprecated: Use errors.IsDeadlockError directly.
func IsDeadlockError(err error) bool {
	return errors.IsDeadlockError(err)
}
