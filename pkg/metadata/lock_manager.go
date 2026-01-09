package metadata

import (
	"sync"
	"time"
)

// LockManager manages byte-range file locks for SMB/NLM protocols.
//
// This is a shared, in-memory implementation that can be embedded in any
// metadata store. Locks are ephemeral and lost on server restart.
//
// Thread Safety:
// LockManager is safe for concurrent use by multiple goroutines.
type LockManager struct {
	mu    sync.RWMutex
	locks map[string][]FileLock // handle key -> locks
}

// NewLockManager creates a new lock manager.
func NewLockManager() *LockManager {
	return &LockManager{
		locks: make(map[string][]FileLock),
	}
}

// Lock attempts to acquire a byte-range lock on a file.
//
// This is a low-level CRUD operation with no permission checking.
// Business logic (permission checks, file type validation) should be
// performed by the caller.
//
// Returns nil on success, or ErrLocked if a conflict exists.
func (lm *LockManager) Lock(handleKey string, lock FileLock) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	existing := lm.locks[handleKey]

	// Check for conflicts with existing locks
	for i := range existing {
		if IsLockConflicting(&existing[i], &lock) {
			conflict := &LockConflict{
				Offset:         existing[i].Offset,
				Length:         existing[i].Length,
				Exclusive:      existing[i].Exclusive,
				OwnerSessionID: existing[i].SessionID,
			}
			return NewLockedError("", conflict)
		}
	}

	// Check if this exact lock already exists (same session, offset, length)
	// If so, update it (allows changing exclusive flag)
	for i := range existing {
		if existing[i].SessionID == lock.SessionID &&
			existing[i].Offset == lock.Offset &&
			existing[i].Length == lock.Length {
			// Update existing lock in place
			existing[i].Exclusive = lock.Exclusive
			existing[i].AcquiredAt = time.Now()
			existing[i].ID = lock.ID
			return nil
		}
	}

	// Set acquisition time if not set
	if lock.AcquiredAt.IsZero() {
		lock.AcquiredAt = time.Now()
	}

	// Add new lock
	lm.locks[handleKey] = append(existing, lock)
	return nil
}

// Unlock releases a specific byte-range lock.
//
// The lock is identified by session, offset, and length - all must match exactly.
//
// Returns nil on success, or ErrLockNotFound if the lock wasn't found.
func (lm *LockManager) Unlock(handleKey string, sessionID, offset, length uint64) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	existing := lm.locks[handleKey]
	if len(existing) == 0 {
		return NewLockNotFoundError("")
	}

	// Find and remove the matching lock
	for i := range existing {
		if existing[i].SessionID == sessionID &&
			existing[i].Offset == offset &&
			existing[i].Length == length {
			// Remove this lock
			lm.locks[handleKey] = append(existing[:i], existing[i+1:]...)

			// Clean up empty entries to prevent memory leak
			if len(lm.locks[handleKey]) == 0 {
				delete(lm.locks, handleKey)
			}
			return nil
		}
	}

	return NewLockNotFoundError("")
}

// UnlockAllForSession releases all locks held by a session on a file.
//
// Returns the number of locks released.
func (lm *LockManager) UnlockAllForSession(handleKey string, sessionID uint64) int {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	existing := lm.locks[handleKey]
	if len(existing) == 0 {
		return 0
	}

	// Filter out locks belonging to this session
	remaining := make([]FileLock, 0, len(existing))
	removed := 0
	for i := range existing {
		if existing[i].SessionID == sessionID {
			removed++
		} else {
			remaining = append(remaining, existing[i])
		}
	}

	// Update or clean up
	if len(remaining) == 0 {
		delete(lm.locks, handleKey)
	} else {
		lm.locks[handleKey] = remaining
	}

	return removed
}

// TestLock checks if a lock would succeed without acquiring it.
//
// Returns (true, nil) if lock would succeed, (false, conflict) if conflict exists.
func (lm *LockManager) TestLock(handleKey string, sessionID, offset, length uint64, exclusive bool) (bool, *LockConflict) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	existing := lm.locks[handleKey]

	// Create a test lock to check conflicts
	testLock := &FileLock{
		SessionID: sessionID,
		Offset:    offset,
		Length:    length,
		Exclusive: exclusive,
	}

	for i := range existing {
		if IsLockConflicting(&existing[i], testLock) {
			return false, &LockConflict{
				Offset:         existing[i].Offset,
				Length:         existing[i].Length,
				Exclusive:      existing[i].Exclusive,
				OwnerSessionID: existing[i].SessionID,
			}
		}
	}

	return true, nil
}

// CheckForIO checks if an I/O operation would conflict with existing locks.
//
// Returns nil if I/O is allowed, or conflict details if blocked.
func (lm *LockManager) CheckForIO(handleKey string, sessionID, offset, length uint64, isWrite bool) *LockConflict {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	existing := lm.locks[handleKey]

	for i := range existing {
		if CheckIOConflict(&existing[i], sessionID, offset, length, isWrite) {
			return &LockConflict{
				Offset:         existing[i].Offset,
				Length:         existing[i].Length,
				Exclusive:      existing[i].Exclusive,
				OwnerSessionID: existing[i].SessionID,
			}
		}
	}

	return nil
}

// ListLocks returns all active locks on a file.
//
// Returns nil if no locks exist.
func (lm *LockManager) ListLocks(handleKey string) []FileLock {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	existing := lm.locks[handleKey]
	if len(existing) == 0 {
		return nil
	}

	// Return a copy to avoid race conditions
	result := make([]FileLock, len(existing))
	copy(result, existing)
	return result
}

// RemoveFileLocks removes all locks for a file.
//
// Called when a file is deleted to clean up any stale lock entries.
func (lm *LockManager) RemoveFileLocks(handleKey string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	delete(lm.locks, handleKey)
}
