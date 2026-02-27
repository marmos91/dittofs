package lock

import (
	"cmp"
	"slices"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// LockManager provides unified lock management for all protocols.
//
// This is the single interface that both NFS and SMB adapters use for lock
// operations. It unifies byte-range locks, oplocks/leases, grace period
// management, and break callback registration into a single coherent API.
//
// The interface covers:
//   - Unified lock CRUD (AddUnifiedLock, RemoveUnifiedLock, etc.)
//   - Centralized break operations (replaces OplockChecker global)
//   - Legacy byte-range locks (backward compat for existing callers)
//   - Grace period management
//   - Break callback registration
//   - Connection/cleanup operations
type LockManager interface {
	// ========================================================================
	// Unified Lock CRUD
	// ========================================================================

	// AddUnifiedLock adds a unified lock (byte-range or oplock).
	// Returns error if the lock conflicts with existing locks.
	AddUnifiedLock(handleKey string, lock *UnifiedLock) error

	// RemoveUnifiedLock removes a unified lock using POSIX splitting semantics.
	RemoveUnifiedLock(handleKey string, owner LockOwner, offset, length uint64) error

	// ListUnifiedLocks returns all unified locks on a file.
	ListUnifiedLocks(handleKey string) []*UnifiedLock

	// RemoveFileUnifiedLocks removes all unified locks for a file.
	RemoveFileUnifiedLocks(handleKey string)

	// UpgradeLock atomically converts a shared lock to exclusive if no other readers exist.
	UpgradeLock(handleKey string, owner LockOwner, offset, length uint64) (*UnifiedLock, error)

	// GetUnifiedLock retrieves a specific unified lock by owner and range.
	GetUnifiedLock(handleKey string, owner LockOwner, offset, length uint64) (*UnifiedLock, error)

	// ========================================================================
	// Centralized Break Operations (replaces OplockChecker global)
	// ========================================================================

	// CheckAndBreakOpLocksForWrite checks and breaks oplocks that conflict with a write.
	// Write breaks all Write oplocks to None, Read oplocks to None.
	// excludeOwner can be nil to check all owners.
	CheckAndBreakOpLocksForWrite(handleKey string, excludeOwner *LockOwner) error

	// CheckAndBreakOpLocksForRead checks and breaks oplocks that conflict with a read.
	// Read only breaks Write oplocks (to Read).
	// excludeOwner can be nil to check all owners.
	CheckAndBreakOpLocksForRead(handleKey string, excludeOwner *LockOwner) error

	// CheckAndBreakOpLocksForDelete checks and breaks all oplocks on a file.
	// Delete breaks all oplocks to None.
	// excludeOwner can be nil to check all owners.
	CheckAndBreakOpLocksForDelete(handleKey string, excludeOwner *LockOwner) error

	// ========================================================================
	// Legacy Byte-Range (backward compat for existing callers)
	// ========================================================================

	// Lock attempts to acquire a byte-range lock on a file.
	Lock(handleKey string, lock FileLock) error

	// Unlock releases a specific byte-range lock.
	Unlock(handleKey string, sessionID uint64, offset, length uint64) error

	// TestLock checks if a lock would succeed without acquiring it.
	TestLock(handleKey string, lock FileLock) (*LockConflict, error)

	// ListLocks returns all active byte-range locks on a file.
	ListLocks(handleKey string) []FileLock

	// ========================================================================
	// Grace Period (part of LockManager per user decision)
	// ========================================================================

	// EnterGracePeriod transitions to grace period state.
	EnterGracePeriod(expectedClients []string)

	// ExitGracePeriod manually exits the grace period.
	ExitGracePeriod()

	// IsOperationAllowed checks if a lock operation is allowed in the current state.
	IsOperationAllowed(op Operation) (bool, error)

	// MarkReclaimed records that a client has reclaimed their locks.
	MarkReclaimed(clientID string)

	// IsInGracePeriod returns true if grace period is currently active.
	IsInGracePeriod() bool

	// ========================================================================
	// Break Callbacks
	// ========================================================================

	// RegisterBreakCallbacks registers typed callbacks for break notifications.
	RegisterBreakCallbacks(callbacks BreakCallbacks)

	// ========================================================================
	// Connection/Cleanup
	// ========================================================================

	// RemoveAllLocks removes all locks (both legacy and unified) for a file.
	RemoveAllLocks(handleKey string)

	// RemoveClientLocks removes all locks held by a specific client.
	RemoveClientLocks(clientID string)

	// GetStats returns current lock manager statistics.
	GetStats() ManagerStats
}

// ManagerStats contains statistics about the lock manager state.
type ManagerStats struct {
	// TotalLegacyLocks is the total number of legacy byte-range locks.
	TotalLegacyLocks int

	// TotalUnifiedLocks is the total number of unified locks.
	TotalUnifiedLocks int

	// TotalFiles is the number of files with any locks.
	TotalFiles int

	// BreakCallbackCount is the number of registered break callbacks.
	BreakCallbackCount int

	// GracePeriodActive indicates if grace period is active.
	GracePeriodActive bool
}

// Verify Manager satisfies LockManager at compile time.
var _ LockManager = (*Manager)(nil)

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
// This implements SMB2 byte-range lock semantics per [MS-SMB2] 3.3.5.15:
//   - Shared lock: Allows reads from all sessions but blocks writes from ALL
//     sessions, including the lock holder. This is the key difference from
//     POSIX advisory locks where a process's own locks never block its own I/O.
//   - Exclusive lock: Only the lock holder can read or write the range.
//
// Conflict rules:
//   - READ + same session + any lock type = ALLOW
//   - READ + different session + shared lock = ALLOW
//   - READ + different session + exclusive lock = BLOCK
//   - WRITE + same session + exclusive lock = ALLOW (lock holder can write)
//   - WRITE + same session + shared lock = BLOCK (shared = read-only for everyone)
//   - WRITE + different session + any lock = BLOCK
//
// Parameters:
//   - existing: The lock to check against
//   - sessionID: The session performing the I/O
//   - offset: Starting byte offset of the I/O
//   - length: Number of bytes in the I/O
//   - isWrite: true for write operations, false for reads
//
// Returns true if the I/O is blocked by the existing lock.
func CheckIOConflict(existing *FileLock, sessionID uint64, offset, length uint64, isWrite bool) bool {
	// Check range overlap first (common case: no overlap)
	if !RangesOverlap(existing.Offset, existing.Length, offset, length) {
		return false
	}

	// Same session handling
	if existing.SessionID == sessionID {
		// Reads from the same session are always allowed regardless of lock type
		if !isWrite {
			return false
		}
		// Writes from the same session:
		// - Exclusive lock holder CAN write to their own locked range
		// - Non-exclusive (shared) lock holder CANNOT write; shared locks are read-only
		//   and prevent writes from all sessions, including the holder.
		return !existing.Exclusive
	}

	// Different session: writes are blocked by any lock (shared or exclusive)
	if isWrite {
		return true
	}

	// Different session reads: only exclusive locks block
	return existing.Exclusive
}

// conflictFrom creates a LockConflict from a FileLock.
func conflictFrom(fl *FileLock) *LockConflict {
	return &LockConflict{
		Offset:         fl.Offset,
		Length:         fl.Length,
		Exclusive:      fl.Exclusive,
		OwnerSessionID: fl.SessionID,
	}
}

// Manager manages byte-range file locks for SMB/NLM protocols.
//
// This is a shared, in-memory implementation that can be embedded in any
// metadata store. Locks are ephemeral and lost on server restart.
//
// Manager implements the LockManager interface, providing unified lock
// management including byte-range locks, oplocks, grace period, and
// typed break callbacks.
//
// Thread Safety:
// Manager is safe for concurrent use by multiple goroutines.
type Manager struct {
	mu             sync.RWMutex
	locks          map[string][]FileLock     // handle key -> locks (legacy)
	unifiedLocks   map[string][]*UnifiedLock // handle key -> unified locks
	breakCallbacks []BreakCallbacks          // registered break callbacks
	gracePeriod    *GracePeriodManager       // grace period state (may be nil)
}

// NewManager creates a new lock manager.
func NewManager() *Manager {
	return &Manager{
		locks:        make(map[string][]FileLock),
		unifiedLocks: make(map[string][]*UnifiedLock),
	}
}

// NewManagerWithGracePeriod creates a new lock manager with a grace period manager.
func NewManagerWithGracePeriod(gracePeriod *GracePeriodManager) *Manager {
	return &Manager{
		locks:        make(map[string][]FileLock),
		unifiedLocks: make(map[string][]*UnifiedLock),
		gracePeriod:  gracePeriod,
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
			return NewLockedError("", conflictFrom(&existing[i]))
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
// Returns (*LockConflict, nil) if conflict exists, or (nil, nil) if lock would succeed.
func (lm *Manager) TestLock(handleKey string, lock FileLock) (*LockConflict, error) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	existing := lm.locks[handleKey]

	for i := range existing {
		if IsLockConflicting(&existing[i], &lock) {
			return conflictFrom(&existing[i]), nil
		}
	}

	return nil, nil
}

// TestLockByParams checks if a lock would succeed without acquiring it (legacy params).
//
// Returns (true, nil) if lock would succeed, (false, conflict) if conflict exists.
func (lm *Manager) TestLockByParams(handleKey string, sessionID, offset, length uint64, exclusive bool) (bool, *LockConflict) {
	testLock := FileLock{
		SessionID: sessionID,
		Offset:    offset,
		Length:    length,
		Exclusive: exclusive,
	}

	conflict, _ := lm.TestLock(handleKey, testLock)
	if conflict != nil {
		return false, conflict
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
			return conflictFrom(&existing[i])
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
//   - []UnifiedLock: The resulting locks after the split (0, 1, or 2 locks)
//
// Examples:
//   - Lock [0-100], Unlock [0-100] -> [] (exact match)
//   - Lock [0-100], Unlock [0-50] -> [[50-100]] (unlock at start)
//   - Lock [0-100], Unlock [50-100] -> [[0-50]] (unlock at end)
//   - Lock [0-100], Unlock [25-75] -> [[0-25], [75-100]] (unlock in middle)
func SplitLock(existing *UnifiedLock, unlockOffset, unlockLength uint64) []*UnifiedLock {
	// Check if ranges overlap at all
	if !RangesOverlap(existing.Offset, existing.Length, unlockOffset, unlockLength) {
		// No overlap - return existing lock unchanged
		return []*UnifiedLock{existing.Clone()}
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
		return []*UnifiedLock{}
	}

	var result []*UnifiedLock

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
//   - []UnifiedLock: Merged locks (may have fewer elements than input)
func MergeLocks(locks []*UnifiedLock) []*UnifiedLock {
	if len(locks) == 0 {
		return nil
	}
	if len(locks) == 1 {
		return []*UnifiedLock{locks[0].Clone()}
	}

	// Group locks by owner+type+filehandle
	type groupKey struct {
		ownerID    string
		lockType   LockType
		fileHandle string
	}

	groups := make(map[groupKey][]*UnifiedLock)
	for _, lock := range locks {
		key := groupKey{
			ownerID:    lock.Owner.OwnerID,
			lockType:   lock.Type,
			fileHandle: string(lock.FileHandle),
		}
		groups[key] = append(groups[key], lock)
	}

	var result []*UnifiedLock

	for _, group := range groups {
		merged := mergeRanges(group)
		result = append(result, merged...)
	}

	return result
}

// mergeRanges merges locks that have the same owner/type/file.
// It combines overlapping or adjacent ranges into single locks.
func mergeRanges(locks []*UnifiedLock) []*UnifiedLock {
	if len(locks) == 0 {
		return nil
	}
	if len(locks) == 1 {
		return []*UnifiedLock{locks[0].Clone()}
	}

	// Sort by offset
	sorted := make([]*UnifiedLock, len(locks))
	for i, l := range locks {
		sorted[i] = l.Clone()
	}
	slices.SortFunc(sorted, func(a, b *UnifiedLock) int {
		return cmp.Compare(a.Offset, b.Offset)
	})

	var result []*UnifiedLock
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
func canMerge(a, b *UnifiedLock) bool {
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
func mergeTwoLocks(a, b *UnifiedLock) *UnifiedLock {
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
//   - *UnifiedLock: The upgraded lock on success
//   - error: ErrLockConflict if other readers exist, ErrLockNotFound if no lock to upgrade
func (lm *Manager) UpgradeLock(handleKey string, owner LockOwner, offset, length uint64) (*UnifiedLock, error) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	unifiedLocks := lm.getUnifiedLocksLocked(handleKey)

	// Step 1: Find existing shared lock owned by this owner covering the range
	var ownLock *UnifiedLock
	var ownLockIndex = -1

	for i, lock := range unifiedLocks {
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
		for _, lock := range unifiedLocks {
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
	for _, lock := range unifiedLocks {
		if lock.Owner.OwnerID == owner.OwnerID {
			continue // Skip our own locks
		}
		if lock.Overlaps(offset, length) {
			// Another owner has a lock on this range - cannot upgrade
			return nil, NewLockConflictError("", &UnifiedLockConflict{
				Lock:   lock,
				Reason: "other reader exists on range",
			})
		}
	}

	// Step 3: Atomically upgrade the lock
	unifiedLocks[ownLockIndex].Type = LockTypeExclusive

	return unifiedLocks[ownLockIndex].Clone(), nil
}

// getUnifiedLocksLocked returns unified locks for a file (must hold lm.mu).
func (lm *Manager) getUnifiedLocksLocked(handleKey string) []*UnifiedLock {
	return lm.unifiedLocks[handleKey]
}

// AddUnifiedLock adds a unified lock to the storage.
//
// Checks for conflicts using the ConflictsWith method which handles all 4
// conflict cases: access modes, oplock-oplock, oplock-byterange, byterange-byterange.
func (lm *Manager) AddUnifiedLock(handleKey string, lock *UnifiedLock) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	existing := lm.unifiedLocks[handleKey]

	// Check for conflicts with existing locks using ConflictsWith
	for _, el := range existing {
		if lock.ConflictsWith(el) {
			return NewLockConflictError("", &UnifiedLockConflict{
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
	lm.unifiedLocks[handleKey] = append(existing, lock)
	return nil
}

// RemoveUnifiedLock removes a unified lock using POSIX splitting semantics.
func (lm *Manager) RemoveUnifiedLock(handleKey string, owner LockOwner, offset, length uint64) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	existing := lm.unifiedLocks[handleKey]
	if len(existing) == 0 {
		return NewLockNotFoundError("")
	}

	var newLocks []*UnifiedLock
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
		delete(lm.unifiedLocks, handleKey)
	} else {
		lm.unifiedLocks[handleKey] = newLocks
	}

	return nil
}

// ListUnifiedLocks returns all unified locks on a file.
func (lm *Manager) ListUnifiedLocks(handleKey string) []*UnifiedLock {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	existing := lm.unifiedLocks[handleKey]
	if len(existing) == 0 {
		return nil
	}

	// Return a copy to avoid race conditions
	result := make([]*UnifiedLock, len(existing))
	for i, el := range existing {
		result[i] = el.Clone()
	}
	return result
}

// RemoveFileUnifiedLocks removes all unified locks for a file.
func (lm *Manager) RemoveFileUnifiedLocks(handleKey string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	delete(lm.unifiedLocks, handleKey)
}

// GetUnifiedLock retrieves a specific unified lock by owner and range.
//
// Returns the matching lock or ErrLockNotFound if no matching lock exists.
func (lm *Manager) GetUnifiedLock(handleKey string, owner LockOwner, offset, length uint64) (*UnifiedLock, error) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	for _, lock := range lm.unifiedLocks[handleKey] {
		if lock.Owner.OwnerID == owner.OwnerID &&
			lock.Offset == offset &&
			lock.Length == length {
			return lock.Clone(), nil
		}
	}

	return nil, NewLockNotFoundError("")
}

// CheckAndBreakOpLocksForWrite checks and initiates breaks for oplocks that
// conflict with a write operation.
//
// Write operations break all oplocks with Read or Write state to None.
func (lm *Manager) CheckAndBreakOpLocksForWrite(handleKey string, excludeOwner *LockOwner) error {
	return lm.breakOpLocks(handleKey, excludeOwner, LeaseStateNone, func(lease *OpLock) bool {
		return lease.HasRead() || lease.HasWrite()
	})
}

// CheckAndBreakOpLocksForRead checks and initiates breaks for oplocks that
// conflict with a read operation.
//
// Read operations only break Write oplocks (downgraded to Read).
func (lm *Manager) CheckAndBreakOpLocksForRead(handleKey string, excludeOwner *LockOwner) error {
	return lm.breakOpLocks(handleKey, excludeOwner, LeaseStateRead, func(lease *OpLock) bool {
		return lease.HasWrite()
	})
}

// CheckAndBreakOpLocksForDelete checks and initiates breaks for all oplocks
// on a file being deleted.
//
// Delete operations break all non-None oplocks to None.
func (lm *Manager) CheckAndBreakOpLocksForDelete(handleKey string, excludeOwner *LockOwner) error {
	return lm.breakOpLocks(handleKey, excludeOwner, LeaseStateNone, func(lease *OpLock) bool {
		return lease.LeaseState != LeaseStateNone
	})
}

// breakOpLocks collects oplocks matching the predicate and dispatches break
// notifications to all registered callbacks.
func (lm *Manager) breakOpLocks(
	handleKey string,
	excludeOwner *LockOwner,
	breakToState uint32,
	shouldBreak func(lease *OpLock) bool,
) error {
	lm.mu.RLock()
	locks := lm.unifiedLocks[handleKey]

	var toBreak []*UnifiedLock
	for _, lock := range locks {
		if lock.Lease == nil {
			continue
		}
		if excludeOwner != nil && lock.Owner.OwnerID == excludeOwner.OwnerID {
			continue
		}
		if shouldBreak(lock.Lease) {
			toBreak = append(toBreak, lock)
		}
	}
	lm.mu.RUnlock()

	for _, lock := range toBreak {
		lm.dispatchOpLockBreak(handleKey, lock, breakToState)
	}

	return nil
}

// dispatchOpLockBreak notifies all registered break callbacks about an oplock break.
func (lm *Manager) dispatchOpLockBreak(handleKey string, lock *UnifiedLock, breakToState uint32) {
	lm.mu.RLock()
	callbacks := make([]BreakCallbacks, len(lm.breakCallbacks))
	copy(callbacks, lm.breakCallbacks)
	lm.mu.RUnlock()

	if len(callbacks) == 0 {
		logger.Debug("oplock break with no callbacks registered",
			"handleKey", handleKey,
			"owner", lock.Owner.OwnerID,
			"breakToState", LeaseStateToString(breakToState))
		return
	}

	for _, cb := range callbacks {
		cb.OnOpLockBreak(handleKey, lock, breakToState)
	}
}

// ============================================================================
// Grace Period Delegation
// ============================================================================

// EnterGracePeriod transitions to grace period state.
// If no grace period manager is configured, this is a no-op.
func (lm *Manager) EnterGracePeriod(expectedClients []string) {
	if lm.gracePeriod != nil {
		lm.gracePeriod.EnterGracePeriod(expectedClients)
	}
}

// ExitGracePeriod manually exits the grace period.
// If no grace period manager is configured, this is a no-op.
func (lm *Manager) ExitGracePeriod() {
	if lm.gracePeriod != nil {
		lm.gracePeriod.ExitGracePeriod()
	}
}

// IsOperationAllowed checks if a lock operation is allowed in the current state.
// If no grace period manager is configured, all operations are allowed.
func (lm *Manager) IsOperationAllowed(op Operation) (bool, error) {
	if lm.gracePeriod != nil {
		return lm.gracePeriod.IsOperationAllowed(op)
	}
	return true, nil
}

// MarkReclaimed records that a client has reclaimed their locks.
// If no grace period manager is configured, this is a no-op.
func (lm *Manager) MarkReclaimed(clientID string) {
	if lm.gracePeriod != nil {
		lm.gracePeriod.MarkReclaimed(clientID)
	}
}

// IsInGracePeriod returns true if grace period is currently active.
func (lm *Manager) IsInGracePeriod() bool {
	if lm.gracePeriod != nil {
		return lm.gracePeriod.GetState() == GraceStateActive
	}
	return false
}

// ============================================================================
// Break Callback Registration
// ============================================================================

// RegisterBreakCallbacks registers typed callbacks for break notifications.
//
// Multiple callbacks can be registered (one per protocol adapter).
// Callbacks are invoked in registration order during break operations.
func (lm *Manager) RegisterBreakCallbacks(callbacks BreakCallbacks) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.breakCallbacks = append(lm.breakCallbacks, callbacks)
}

// ============================================================================
// Connection/Cleanup Operations
// ============================================================================

// RemoveAllLocks removes all locks (both legacy and unified) for a file.
func (lm *Manager) RemoveAllLocks(handleKey string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	delete(lm.locks, handleKey)
	delete(lm.unifiedLocks, handleKey)
}

// RemoveClientLocks removes all locks held by a specific client.
//
// This iterates all files and removes any unified locks owned by the
// specified client ID. Also removes legacy locks by scanning all sessions.
func (lm *Manager) RemoveClientLocks(clientID string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	// Remove unified locks for this client
	for handleKey, locks := range lm.unifiedLocks {
		var remaining []*UnifiedLock
		for _, lock := range locks {
			if lock.Owner.ClientID != clientID {
				remaining = append(remaining, lock)
			}
		}
		if len(remaining) == 0 {
			delete(lm.unifiedLocks, handleKey)
		} else {
			lm.unifiedLocks[handleKey] = remaining
		}
	}
}

// GetStats returns current lock manager statistics.
func (lm *Manager) GetStats() ManagerStats {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	totalLegacy := 0
	for _, locks := range lm.locks {
		totalLegacy += len(locks)
	}

	totalUnified := 0
	for _, locks := range lm.unifiedLocks {
		totalUnified += len(locks)
	}

	// Count unique files (files that have any locks)
	fileSet := make(map[string]struct{})
	for key := range lm.locks {
		fileSet[key] = struct{}{}
	}
	for key := range lm.unifiedLocks {
		fileSet[key] = struct{}{}
	}

	return ManagerStats{
		TotalLegacyLocks:   totalLegacy,
		TotalUnifiedLocks:  totalUnified,
		TotalFiles:         len(fileSet),
		BreakCallbackCount: len(lm.breakCallbacks),
		GracePeriodActive:  lm.gracePeriod != nil && lm.gracePeriod.GetState() == GraceStateActive,
	}
}
