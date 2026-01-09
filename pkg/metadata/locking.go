package metadata

import (
	"sync"
	"time"
)

// ============================================================================
// File Locking Types (SMB/NLM support)
// ============================================================================

// FileLock represents a byte-range lock on a file.
//
// Byte-range locks control what portions of a file can be read/written while
// locked by other clients. They are used by SMB2 LOCK command and NFS NLM protocol.
//
// Lock Types:
//   - Exclusive (write): No other locks allowed on overlapping range
//   - Shared (read): Multiple shared locks allowed, no exclusive locks
//
// Lock Lifetime:
// Locks are advisory and ephemeral (in-memory only). They persist until:
//   - Explicitly released via UnlockFile
//   - File is closed (UnlockAllForSession)
//   - Session disconnects (cleanup all session locks)
//   - Server restarts (all locks lost)
type FileLock struct {
	// ID is the lock identifier from the client.
	// For SMB2: derived from lock request (often 0 for simple locks)
	// For NLM: opaque client-provided lock handle
	ID uint64

	// SessionID identifies who holds the lock.
	// For SMB2: SessionID from SMB header
	// For NLM: hash of network address + client PID
	SessionID uint64

	// Offset is the starting byte offset of the lock.
	Offset uint64

	// Length is the number of bytes locked.
	// 0 means "to end of file" (unbounded).
	Length uint64

	// Exclusive indicates lock type.
	// true = exclusive (write lock, blocks all other locks)
	// false = shared (read lock, allows other shared locks)
	Exclusive bool

	// AcquiredAt is the time the lock was acquired.
	AcquiredAt time.Time

	// ClientAddr is the network address of the client holding the lock.
	// Used for debugging and logging.
	ClientAddr string
}

// LockConflict describes a conflicting lock for error reporting.
//
// When LockFile or TestLock fails due to a conflict, this structure
// provides information about the conflicting lock. This can be used
// by protocols to report conflict details back to clients.
type LockConflict struct {
	// Offset is the starting byte offset of the conflicting lock.
	Offset uint64

	// Length is the number of bytes of the conflicting lock.
	Length uint64

	// Exclusive indicates type of conflicting lock.
	Exclusive bool

	// OwnerSessionID identifies the client holding the conflicting lock.
	OwnerSessionID uint64
}

// ============================================================================
// Lock Range Utilities
// ============================================================================

// RangesOverlap checks if two byte ranges overlap.
//
// A range is defined by (offset, length) where length=0 means "to EOF".
// Two ranges overlap if they share at least one byte position.
//
// Examples:
//   - (0, 10) and (5, 10): overlap at bytes 5-9
//   - (0, 10) and (10, 10): no overlap (ranges are adjacent)
//   - (0, 0) and (100, 10): overlap (first range is unbounded)
//   - (0, 10) and (0, 0): overlap (second range is unbounded)
func RangesOverlap(offset1, length1, offset2, length2 uint64) bool {
	// Handle "to EOF" (length=0) cases
	// If either range extends to EOF, it overlaps with anything at or after its start
	if length1 == 0 && length2 == 0 {
		// Both are unbounded - always overlap
		return true
	}
	if length1 == 0 {
		// First range is unbounded, second is bounded
		// Overlap if second range starts at or after first, or extends into first
		end2 := offset2 + length2
		return offset2 >= offset1 || end2 > offset1
	}
	if length2 == 0 {
		// Second range is unbounded, first is bounded
		// Overlap if first range starts at or after second, or extends into second
		end1 := offset1 + length1
		return offset1 >= offset2 || end1 > offset2
	}

	// Both ranges are bounded
	// No overlap if one range ends before the other starts
	end1 := offset1 + length1
	end2 := offset2 + length2

	// Ranges don't overlap if: offset1 >= end2 OR offset2 >= end1
	// They DO overlap if: offset1 < end2 AND offset2 < end1
	return offset1 < end2 && offset2 < end1
}

// IsLockConflicting checks if two locks conflict with each other.
//
// Conflict rules:
//   - Shared locks don't conflict with other shared locks (multiple readers)
//   - Exclusive locks conflict with all other locks
//   - Locks from the same session don't conflict (allows re-locking same range)
//   - Ranges must overlap for a conflict to occur
//
// Same-session re-locking: When a session requests a lock on a range it already
// holds, there is no conflict. This enables changing lock type on a range
// (e.g., shared -> exclusive) by acquiring a new lock that replaces the old one.
func IsLockConflicting(existing, requested *FileLock) bool {
	// Same session - no conflict (allows re-locking same range with different type)
	if existing.SessionID == requested.SessionID {
		return false
	}

	// Check range overlap first (common case: no overlap)
	if !RangesOverlap(existing.Offset, existing.Length, requested.Offset, requested.Length) {
		return false
	}

	// Both shared (read) locks - no conflict
	if !existing.Exclusive && !requested.Exclusive {
		return false
	}

	// At least one is exclusive and ranges overlap - conflict
	return true
}

// CheckIOConflict checks if an I/O operation conflicts with an existing lock.
//
// This is used to verify READ/WRITE operations don't violate locks:
//   - READ is blocked by another session's exclusive lock
//   - WRITE is blocked by any other session's lock (shared or exclusive)
//
// Parameters:
//   - existing: The lock to check against
//   - sessionID: The session performing the I/O (their own locks don't block them)
//   - offset: Starting byte offset of the I/O
//   - length: Number of bytes in the I/O
//   - isWrite: true for write operations, false for reads
//
// Returns true if the I/O is blocked by the existing lock.
func CheckIOConflict(existing *FileLock, sessionID uint64, offset, length uint64, isWrite bool) bool {
	// Same session - no conflict
	if existing.SessionID == sessionID {
		return false
	}

	// Check range overlap
	if !RangesOverlap(existing.Offset, existing.Length, offset, length) {
		return false
	}

	// For writes: any lock blocks (shared or exclusive)
	if isWrite {
		return true
	}

	// For reads: only exclusive locks block
	return existing.Exclusive
}

// ============================================================================
// Lock Manager
// ============================================================================

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
