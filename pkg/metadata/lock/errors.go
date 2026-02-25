package lock

import (
	"github.com/marmos91/dittofs/pkg/metadata/errors"
)

// ============================================================================
// Lock Error Factory Functions
//
// These functions create lock-specific errors using the generic errors package.
// They provide convenient constructors for common lock error scenarios.
// ============================================================================

// NewLockedError creates error for lock conflicts (legacy FileLock).
func NewLockedError(path string, conflict *LockConflict) *errors.StoreError {
	msg := "resource is locked"
	if conflict != nil {
		msg = "resource is locked by another session"
	}
	return &errors.StoreError{
		Code:    errors.ErrLocked,
		Message: msg,
		Path:    path,
	}
}

// NewLockNotFoundError creates error for missing locks.
func NewLockNotFoundError(path string) *errors.StoreError {
	return &errors.StoreError{
		Code:    errors.ErrLockNotFound,
		Message: "lock not found",
		Path:    path,
	}
}

// NewLockConflictError creates error for enhanced lock conflicts.
func NewLockConflictError(path string, conflict *UnifiedLockConflict) *errors.StoreError {
	msg := "lock conflict"
	if conflict != nil && conflict.Reason != "" {
		msg = conflict.Reason
	}
	return &errors.StoreError{
		Code:    errors.ErrLockConflict,
		Message: msg,
		Path:    path,
	}
}

// NewDeadlockError creates error for deadlock detection.
func NewDeadlockError(waiter string, blockedBy []string) *errors.StoreError {
	return &errors.StoreError{
		Code:    errors.ErrDeadlock,
		Message: "deadlock detected",
		Path:    waiter,
	}
}

// NewGracePeriodError creates error for grace period blocking.
func NewGracePeriodError(remainingSeconds int) *errors.StoreError {
	return &errors.StoreError{
		Code:    errors.ErrGracePeriod,
		Message: "grace period active, new locks blocked",
	}
}

// NewLockLimitExceededError creates error for lock limit violations.
func NewLockLimitExceededError(limitType string, current, max int) *errors.StoreError {
	return &errors.StoreError{
		Code:    errors.ErrLockLimitExceeded,
		Message: limitType + " lock limit exceeded",
	}
}
