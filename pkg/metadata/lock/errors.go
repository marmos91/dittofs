package lock

import (
	stderrors "errors"

	"github.com/marmos91/dittofs/pkg/metadata/errors"
)

// isLockNotFound reports whether err is a store "lock not found" error. Used by
// the synchronous delete path to ignore an already-absent record (idempotent
// delete) without logging a spurious durability alarm.
func isLockNotFound(err error) bool {
	var se *errors.StoreError
	if stderrors.As(err, &se) {
		return se.Code == errors.ErrLockNotFound
	}
	return false
}

// NewLockedError creates error for lock conflicts (legacy FileLock).
func NewLockedError(path string, conflict *LockConflict) *errors.StoreError {
	msg := "resource is locked"
	var ownerID string
	if conflict != nil {
		msg = "resource is locked by another session"
		ownerID = conflict.OwnerID
	}
	return &errors.StoreError{
		Code:            errors.ErrLocked,
		Message:         msg,
		Path:            path,
		ConflictOwnerID: ownerID,
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

// NewLockConflictError creates error for unified lock conflicts.
func NewLockConflictError(path string, conflict *UnifiedLockConflict) *errors.StoreError {
	msg := "lock conflict"
	var ownerID string
	if conflict != nil {
		if conflict.Reason != "" {
			msg = conflict.Reason
		}
		if conflict.Lock != nil {
			ownerID = conflict.Lock.Owner.OwnerID
		}
	}
	return &errors.StoreError{
		Code:            errors.ErrLockConflict,
		Message:         msg,
		Path:            path,
		ConflictOwnerID: ownerID,
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
