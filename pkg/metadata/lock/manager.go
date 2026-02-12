package lock

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

// Manager manages byte-range file locks for SMB/NLM protocols.
//
// This is a shared, in-memory implementation that can be embedded in any
// metadata store. Locks are ephemeral and lost on server restart.
//
// Thread Safety:
// Manager is safe for concurrent use by multiple goroutines.
type Manager struct {
	mu            sync.RWMutex
	locks         map[string][]FileLock      // handle key -> locks (legacy)
	enhancedLocks map[string][]*EnhancedLock // handle key -> enhanced locks
}

// NewManager creates a new lock manager.
func NewManager() *Manager {
	return &Manager{
		locks:         make(map[string][]FileLock),
		enhancedLocks: make(map[string][]*EnhancedLock),
	}
}

// Lock attempts to acquire a byte-range lock on a file.
//
// This is a low-level CRUD operation with no permission checking.
// Business logic (permission checks, file type validation) should be
// performed by the caller.
//
// Returns nil on success, or ErrLocked if a conflict exists.
func (lm *Manager) Lock(handleKey string, lock FileLock) error {
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
func (lm *Manager) Unlock(handleKey string, sessionID, offset, length uint64) error {
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
func (lm *Manager) UnlockAllForSession(handleKey string, sessionID uint64) int {
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
func (lm *Manager) TestLock(handleKey string, sessionID, offset, length uint64, exclusive bool) (bool, *LockConflict) {
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
func (lm *Manager) CheckForIO(handleKey string, sessionID, offset, length uint64, isWrite bool) *LockConflict {
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
func (lm *Manager) ListLocks(handleKey string) []FileLock {
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
func (lm *Manager) RemoveFileLocks(handleKey string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	delete(lm.locks, handleKey)
}

// ============================================================================
// POSIX Lock Splitting
// ============================================================================

// SplitLock splits an existing lock when a portion is unlocked.
//
// POSIX semantics require that unlocking a portion of a locked range results in:
//   - 0 locks: if the unlock range covers the entire lock
//   - 1 lock: if the unlock range covers the start or end
//   - 2 locks: if the unlock range is in the middle (creates a "hole")
//
// Parameters:
//   - existing: The lock to split
//   - unlockOffset: Starting byte offset of the unlock range
//   - unlockLength: Number of bytes to unlock (0 = to EOF)
//
// Returns:
//   - []EnhancedLock: The resulting locks after the split (0, 1, or 2 locks)
//
// Examples:
//   - Lock [0-100], Unlock [0-100] -> [] (exact match)
//   - Lock [0-100], Unlock [0-50] -> [[50-100]] (unlock at start)
//   - Lock [0-100], Unlock [50-100] -> [[0-50]] (unlock at end)
//   - Lock [0-100], Unlock [25-75] -> [[0-25], [75-100]] (unlock in middle)
func SplitLock(existing *EnhancedLock, unlockOffset, unlockLength uint64) []*EnhancedLock {
	// Check if ranges overlap at all
	if !RangesOverlap(existing.Offset, existing.Length, unlockOffset, unlockLength) {
		// No overlap - return existing lock unchanged
		return []*EnhancedLock{existing.Clone()}
	}

	// Calculate lock end
	lockEnd := existing.End()
	if existing.Length == 0 {
		// Unbounded lock - treat as very large for calculation purposes
		lockEnd = ^uint64(0) // Max uint64
	}

	// Calculate unlock end
	unlockEnd := unlockOffset + unlockLength
	if unlockLength == 0 {
		// Unbounded unlock - goes to EOF
		unlockEnd = ^uint64(0)
	}

	// Check for exact match or complete coverage
	if unlockOffset <= existing.Offset && unlockEnd >= lockEnd {
		// Unlock completely covers the lock - remove it
		return []*EnhancedLock{}
	}

	var result []*EnhancedLock

	// Check if there's a portion before the unlock range
	if unlockOffset > existing.Offset {
		beforeLock := existing.Clone()
		beforeLock.Length = unlockOffset - existing.Offset
		result = append(result, beforeLock)
	}

	// Check if there's a portion after the unlock range
	if unlockEnd < lockEnd {
		afterLock := existing.Clone()
		afterLock.Offset = unlockEnd
		if existing.Length == 0 {
			// Original was unbounded, after portion is also unbounded
			afterLock.Length = 0
		} else {
			afterLock.Length = lockEnd - unlockEnd
		}
		result = append(result, afterLock)
	}

	return result
}

// ============================================================================
// Lock Merging
// ============================================================================

// MergeLocks coalesces adjacent or overlapping locks from the same owner.
//
// This is used when upgrading or extending locks to avoid fragmentation.
// Only locks with the same owner, type, and file handle can be merged.
//
// Parameters:
//   - locks: Slice of locks to potentially merge
//
// Returns:
//   - []EnhancedLock: Merged locks (may have fewer elements than input)
func MergeLocks(locks []*EnhancedLock) []*EnhancedLock {
	if len(locks) == 0 {
		return nil
	}
	if len(locks) == 1 {
		return []*EnhancedLock{locks[0].Clone()}
	}

	// Group locks by owner+type+filehandle
	type groupKey struct {
		ownerID    string
		lockType   LockType
		fileHandle string
	}

	groups := make(map[groupKey][]*EnhancedLock)
	for _, lock := range locks {
		key := groupKey{
			ownerID:    lock.Owner.OwnerID,
			lockType:   lock.Type,
			fileHandle: string(lock.FileHandle),
		}
		groups[key] = append(groups[key], lock)
	}

	var result []*EnhancedLock

	for _, group := range groups {
		merged := mergeRanges(group)
		result = append(result, merged...)
	}

	return result
}

// mergeRanges merges locks that have the same owner/type/file.
// It combines overlapping or adjacent ranges into single locks.
func mergeRanges(locks []*EnhancedLock) []*EnhancedLock {
	if len(locks) == 0 {
		return nil
	}
	if len(locks) == 1 {
		return []*EnhancedLock{locks[0].Clone()}
	}

	// Sort by offset (simple bubble sort for small slices)
	sorted := make([]*EnhancedLock, len(locks))
	for i, l := range locks {
		sorted[i] = l.Clone()
	}
	for i := 0; i < len(sorted)-1; i++ {
		for j := 0; j < len(sorted)-i-1; j++ {
			if sorted[j].Offset > sorted[j+1].Offset {
				sorted[j], sorted[j+1] = sorted[j+1], sorted[j]
			}
		}
	}

	var result []*EnhancedLock
	current := sorted[0]

	for i := 1; i < len(sorted); i++ {
		next := sorted[i]

		// Check if current and next can be merged
		if canMerge(current, next) {
			// Merge into current
			current = mergeTwoLocks(current, next)
		} else {
			// Can't merge - finalize current and move to next
			result = append(result, current)
			current = next
		}
	}

	// Don't forget the last one
	result = append(result, current)

	return result
}

// canMerge checks if two locks can be merged (adjacent or overlapping).
func canMerge(a, b *EnhancedLock) bool {
	// Must be same owner, type, and file (assumed by caller grouping)

	// Handle unbounded locks
	if a.Length == 0 {
		// a is unbounded - can merge with anything at or after a.Offset
		return b.Offset >= a.Offset
	}
	if b.Length == 0 {
		// b is unbounded - can merge if a overlaps or is adjacent to b.Offset
		return a.End() >= b.Offset
	}

	// Both bounded - check if adjacent or overlapping
	aEnd := a.End()
	return aEnd >= b.Offset // Adjacent (aEnd == b.Offset) or overlapping
}

// mergeTwoLocks combines two locks into one.
func mergeTwoLocks(a, b *EnhancedLock) *EnhancedLock {
	result := a.Clone()

	// Start is the minimum offset
	if b.Offset < result.Offset {
		result.Offset = b.Offset
	}

	// Handle unbounded locks
	if a.Length == 0 || b.Length == 0 {
		result.Length = 0 // Result is unbounded
		return result
	}

	// Both bounded - end is the maximum
	aEnd := a.End()
	bEnd := b.End()
	maxEnd := aEnd
	if bEnd > maxEnd {
		maxEnd = bEnd
	}

	result.Length = maxEnd - result.Offset
	return result
}

// ============================================================================
// Atomic Lock Upgrade
// ============================================================================

// UpgradeLock atomically converts a shared lock to exclusive if no other readers exist.
//
// This implements the user decision: "Lock upgrade: Atomic upgrade supported
// (read -> write if no other readers)".
//
// Steps:
//  1. Find existing shared lock owned by `owner` covering the range
//  2. Check if any OTHER owners hold shared locks on overlapping range
//  3. If other readers exist: return ErrLockConflict
//  4. If no other readers: atomically change lock type to Exclusive
//
// Parameters:
//   - handleKey: The file handle key
//   - owner: The lock owner requesting the upgrade
//   - offset: Starting byte offset of the range to upgrade
//   - length: Number of bytes (0 = to EOF)
//
// Returns:
//   - *EnhancedLock: The upgraded lock on success
//   - error: ErrLockConflict if other readers exist, ErrLockNotFound if no lock to upgrade
func (lm *Manager) UpgradeLock(handleKey string, owner LockOwner, offset, length uint64) (*EnhancedLock, error) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	// This method works with the enhanced lock storage (if available)
	// For now, we'll add enhanced lock storage alongside the existing FileLock storage

	enhancedLocks := lm.getEnhancedLocksLocked(handleKey)

	// Step 1: Find existing shared lock owned by this owner covering the range
	var ownLock *EnhancedLock
	var ownLockIndex = -1

	for i, lock := range enhancedLocks {
		if lock.Owner.OwnerID == owner.OwnerID &&
			lock.Type == LockTypeShared &&
			lock.Overlaps(offset, length) {
			// Found our shared lock
			ownLock = lock
			ownLockIndex = i
			break
		}
	}

	if ownLock == nil {
		// Check if we already have an exclusive lock (no-op case)
		for _, lock := range enhancedLocks {
			if lock.Owner.OwnerID == owner.OwnerID &&
				lock.Type == LockTypeExclusive &&
				lock.Overlaps(offset, length) {
				// Already exclusive - return it as-is
				return lock.Clone(), nil
			}
		}
		return nil, NewLockNotFoundError("")
	}

	// Step 2: Check if any OTHER owners hold shared locks on overlapping range
	for _, lock := range enhancedLocks {
		if lock.Owner.OwnerID == owner.OwnerID {
			continue // Skip our own locks
		}
		if lock.Overlaps(offset, length) {
			// Another owner has a lock on this range - cannot upgrade
			return nil, NewLockConflictError("", &EnhancedLockConflict{
				Lock:   lock,
				Reason: "other reader exists on range",
			})
		}
	}

	// Step 3: Atomically upgrade the lock
	enhancedLocks[ownLockIndex].Type = LockTypeExclusive

	return enhancedLocks[ownLockIndex].Clone(), nil
}

// getEnhancedLocksLocked returns enhanced locks for a file (must hold lm.mu).
func (lm *Manager) getEnhancedLocksLocked(handleKey string) []*EnhancedLock {
	return lm.enhancedLocks[handleKey]
}

// AddEnhancedLock adds an enhanced lock to the storage.
func (lm *Manager) AddEnhancedLock(handleKey string, lock *EnhancedLock) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	existing := lm.enhancedLocks[handleKey]

	// Check for conflicts with existing locks
	for _, el := range existing {
		if IsEnhancedLockConflicting(el, lock) {
			return NewLockConflictError("", &EnhancedLockConflict{
				Lock:   el,
				Reason: "lock conflict",
			})
		}
	}

	// Check if this exact lock already exists (same owner, offset, length)
	// If so, update it (allows changing lock type)
	for i, el := range existing {
		if el.Owner.OwnerID == lock.Owner.OwnerID &&
			el.Offset == lock.Offset &&
			el.Length == lock.Length {
			// Update existing lock in place
			existing[i].Type = lock.Type
			existing[i].AcquiredAt = time.Now()
			return nil
		}
	}

	// Set acquisition time if not set
	if lock.AcquiredAt.IsZero() {
		lock.AcquiredAt = time.Now()
	}

	// Add new lock
	lm.enhancedLocks[handleKey] = append(existing, lock)
	return nil
}

// RemoveEnhancedLock removes an enhanced lock using POSIX splitting semantics.
func (lm *Manager) RemoveEnhancedLock(handleKey string, owner LockOwner, offset, length uint64) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	existing := lm.enhancedLocks[handleKey]
	if len(existing) == 0 {
		return NewLockNotFoundError("")
	}

	var newLocks []*EnhancedLock
	found := false

	for _, lock := range existing {
		if lock.Owner.OwnerID != owner.OwnerID {
			// Not our lock - keep it
			newLocks = append(newLocks, lock)
			continue
		}

		// Our lock - check if it overlaps with the unlock range
		if !lock.Overlaps(offset, length) {
			// Doesn't overlap - keep it unchanged
			newLocks = append(newLocks, lock)
			continue
		}

		// Overlaps - split the lock
		found = true
		splitResult := SplitLock(lock, offset, length)
		newLocks = append(newLocks, splitResult...)
	}

	if !found {
		return NewLockNotFoundError("")
	}

	// Update or clean up
	if len(newLocks) == 0 {
		delete(lm.enhancedLocks, handleKey)
	} else {
		lm.enhancedLocks[handleKey] = newLocks
	}

	return nil
}

// ListEnhancedLocks returns all enhanced locks on a file.
func (lm *Manager) ListEnhancedLocks(handleKey string) []*EnhancedLock {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	existing := lm.enhancedLocks[handleKey]
	if len(existing) == 0 {
		return nil
	}

	// Return a copy to avoid race conditions
	result := make([]*EnhancedLock, len(existing))
	for i, el := range existing {
		result[i] = el.Clone()
	}
	return result
}

// RemoveEnhancedFileLocks removes all enhanced locks for a file.
func (lm *Manager) RemoveEnhancedFileLocks(handleKey string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	delete(lm.enhancedLocks, handleKey)
}
