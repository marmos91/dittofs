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
// the lock manager raise and inspect the same errors without a cycle — the
// store backends import pkg/metadata/errors directly — while the aliases here
// keep the API reachable from the package that owns it.
//
// These are aliases, not wrappers: each name here and the name it points at
// denote one and the same type, so a value can be passed to anything
// expecting either spelling without conversion. Removing or renaming a name
// here breaks every caller of the metadata package; it is not the cleanup of
// an unused shim.

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
	ErrCrossShare             = errors.ErrCrossShare
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

// NewPermissionDeniedError creates a StoreError for permission denied errors.
func NewPermissionDeniedError(path string) *StoreError {
	return errors.NewPermissionDeniedError(path)
}

// NewIsDirectoryError creates a StoreError for when a file operation is attempted on a directory.
func NewIsDirectoryError(path string) *StoreError {
	return errors.NewIsDirectoryError(path)
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

// NewInvalidArgumentError creates a StoreError for invalid arguments.
func NewInvalidArgumentError(message string) *StoreError {
	return errors.NewInvalidArgumentError(message)
}

// NewLockedError creates a StoreError for lock conflicts.
func NewLockedError(path string, conflict *LockConflict) *StoreError {
	return lock.NewLockedError(path, conflict)
}

// ============================================================================
// Error Helper Functions
// ============================================================================

// IsNotFoundError checks if an error is a StoreError with ErrNotFound code.
func IsNotFoundError(err error) bool {
	return errors.IsNotFoundError(err)
}

// IsInvalidHandleError checks if an error is a StoreError with ErrInvalidHandle code.
func IsInvalidHandleError(err error) bool {
	return errors.IsInvalidHandleError(err)
}

// IsStaleHandleError checks if an error is a StoreError with ErrStaleHandle code.
func IsStaleHandleError(err error) bool {
	return errors.IsStaleHandleError(err)
}

// ============================================================================
// Error Helper Functions
// ============================================================================

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
