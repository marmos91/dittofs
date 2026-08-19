package metadata

import (
	"github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// Public error surface of package metadata
// ============================================================================
//
// The names below are package metadata's error API. Callers across the tree —
// every protocol adapter, the control-plane API handlers, the block engine and
// all four metadata store backends — construct, compare and type-switch on
// them as metadata.StoreError, metadata.ErrNotFound, metadata.IsNotFoundError
// and the rest.
//
// The implementations live in pkg/metadata/errors and pkg/metadata/lock
// because neither of those packages may import metadata: metadata imports
// both, so the dependency only runs one way. That lets the store backends and
// the lock manager raise and inspect the same errors without a cycle — all
// four backends import pkg/metadata/errors directly — while the aliases here
// keep the API reachable from the package that owns it.
//
// These are aliases, not wrappers, so a value built through either spelling is
// the identical type and compares equal. Removing or renaming a name here
// breaks every caller of the metadata package; it is not the cleanup of an
// unused shim.

// StoreError is re-exported from the errors package.
type StoreError = errors.StoreError

// ErrorCode is re-exported from the errors package.
type ErrorCode = errors.ErrorCode

// The error codes callers compare StoreError.Code against.
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
	ErrConflict               = errors.ErrConflict
)

// ============================================================================
// Lock types, re-exported so lock-aware errors stay reachable here
// ============================================================================

// LockConflict is re-exported from the lock package.
type LockConflict = lock.LockConflict

// UnifiedLockConflict is re-exported from the lock package.
type UnifiedLockConflict = lock.UnifiedLockConflict

// ============================================================================
// Error Factory Functions
// ============================================================================

// NewNotFoundError creates a StoreError for when a file, directory, or share is not found.
func NewNotFoundError(path string, entityType string) *StoreError {
	return errors.NewNotFoundError(path, entityType)
}

// NewPermissionDeniedError creates a StoreError for permission denied errors.
func NewPermissionDeniedError(path string) *StoreError {
	return errors.NewPermissionDeniedError(path)
}

// NewIsDirectoryError creates a StoreError for when a file operation is attempted on a directory.
func NewIsDirectoryError(path string) *StoreError {
	return errors.NewIsDirectoryError(path)
}

// NewNotDirectoryError creates a StoreError for when a directory operation is attempted on a non-directory.
func NewNotDirectoryError(path string) *StoreError {
	return errors.NewNotDirectoryError(path)
}

// NewInvalidHandleError creates a StoreError for malformed file handles.
func NewInvalidHandleError() *StoreError {
	return errors.NewInvalidHandleError()
}

// NewStaleHandleError creates a StoreError for handles that decode but name a
// share that no longer exists.
func NewStaleHandleError(shareName string) *StoreError {
	return errors.NewStaleHandleError(shareName)
}

// NewNotEmptyError creates a StoreError for when a directory is not empty.
func NewNotEmptyError(path string) *StoreError {
	return errors.NewNotEmptyError(path)
}

// NewAlreadyExistsError creates a StoreError for when a file/directory already exists.
func NewAlreadyExistsError(path string) *StoreError {
	return errors.NewAlreadyExistsError(path)
}

// NewConflictError creates a StoreError for ObjectID concurrent-write conflicts.
func NewConflictError(op, message string) *StoreError {
	return errors.NewConflictError(op, message)
}

// NewInvalidArgumentError creates a StoreError for invalid arguments.
func NewInvalidArgumentError(message string) *StoreError {
	return errors.NewInvalidArgumentError(message)
}

// NewAccessDeniedError creates a StoreError for share-level access denial.
func NewAccessDeniedError(reason string) *StoreError {
	return errors.NewAccessDeniedError(reason)
}

// NewLockedError creates a StoreError for lock conflicts.
func NewLockedError(path string, conflict *LockConflict) *StoreError {
	return lock.NewLockedError(path, conflict)
}

// NewLockNotFoundError creates a StoreError for unlock operations on non-existent locks.
func NewLockNotFoundError(path string) *StoreError {
	return lock.NewLockNotFoundError(path)
}

// NewQuotaExceededError creates a StoreError for quota exceeded errors.
func NewQuotaExceededError(path string) *StoreError {
	return errors.NewQuotaExceededError(path)
}

// NewPrivilegeRequiredError creates a StoreError for operations requiring root.
func NewPrivilegeRequiredError(operation string) *StoreError {
	return errors.NewPrivilegeRequiredError(operation)
}

// NewNameTooLongError creates a StoreError for paths/names exceeding limits.
func NewNameTooLongError(path string) *StoreError {
	return errors.NewNameTooLongError(path)
}

// NewDeadlockError creates a StoreError for deadlock detection.
func NewDeadlockError(waiter string, blockedBy []string) *StoreError {
	return lock.NewDeadlockError(waiter, blockedBy)
}

// NewGracePeriodError creates a StoreError for grace period blocking.
func NewGracePeriodError(remainingSeconds int) *StoreError {
	return lock.NewGracePeriodError(remainingSeconds)
}

// NewLockLimitExceededError creates a StoreError for lock limit violations.
func NewLockLimitExceededError(limitType string, current, max int) *StoreError {
	return lock.NewLockLimitExceededError(limitType, current, max)
}

// NewLockConflictError creates a StoreError for lock conflicts (upgrade, etc.).
func NewLockConflictError(path string, conflict *UnifiedLockConflict) *StoreError {
	return lock.NewLockConflictError(path, conflict)
}

// ============================================================================
// Error Helper Functions
// ============================================================================

// IsNotFoundError checks if an error is a StoreError with ErrNotFound code.
func IsNotFoundError(err error) bool {
	return errors.IsNotFoundError(err)
}

// IsLockConflictError checks if an error is a StoreError with ErrLockConflict code.
func IsLockConflictError(err error) bool {
	return errors.IsLockConflictError(err)
}

// IsDeadlockError checks if an error is a StoreError with ErrDeadlock code.
func IsDeadlockError(err error) bool {
	return errors.IsDeadlockError(err)
}

// IsConflictError checks if an error is a StoreError with ErrConflict code.
func IsConflictError(err error) bool {
	return errors.IsConflictError(err)
}

// IsInvalidHandleError checks if an error is a StoreError with ErrInvalidHandle code.
func IsInvalidHandleError(err error) bool {
	return errors.IsInvalidHandleError(err)
}

// IsStaleHandleError checks if an error is a StoreError with ErrStaleHandle code.
func IsStaleHandleError(err error) bool {
	return errors.IsStaleHandleError(err)
}
