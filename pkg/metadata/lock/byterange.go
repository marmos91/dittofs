package lock

import (
	"cmp"
	"slices"
	"time"

	"github.com/google/uuid"
)

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

	// Calculate lock end. End() returns maxUint64 for unbounded (Length==0)
	// locks, which is exactly the "treat as very large" value the coverage
	// arithmetic below needs.
	lockEnd := existing.End()

	// Calculate unlock end. unlockOffset/unlockLength are wire-controlled, so
	// the sum can wrap uint64 (e.g. offset near 2^64 + a large length); a wrap
	// would corrupt the coverage checks below (fabricate or drop a fragment).
	// Clamp to maxUint64 on overflow — a range that reaches past 2^64 covers to
	// EOF for our purposes, same as an unbounded unlock.
	unlockEnd := unlockOffset + unlockLength
	if unlockLength == 0 || unlockEnd < unlockOffset {
		// Unbounded unlock (length 0 = to EOF) or arithmetic overflow.
		unlockEnd = ^uint64(0)
	}

	// Check for exact match or complete coverage
	if unlockOffset <= existing.Offset && unlockEnd >= lockEnd {
		// Unlock completely covers the lock - remove it
		return []*UnifiedLock{}
	}

	var result []*UnifiedLock

	// Each fragment is a distinct lock and MUST get a fresh ID. Clone() copies
	// the original's ID verbatim, so two fragments would otherwise share one
	// persist identity — the second PutLock would overwrite the first (the
	// store is keyed by ID), silently losing one byte-range across a restart.
	// Check if there's a portion before the unlock range
	if unlockOffset > existing.Offset {
		beforeLock := existing.Clone()
		beforeLock.ID = uuid.New().String()
		beforeLock.Length = unlockOffset - existing.Offset
		result = append(result, beforeLock)
	}

	// Check if there's a portion after the unlock range
	if unlockEnd < lockEnd {
		afterLock := existing.Clone()
		afterLock.ID = uuid.New().String()
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
	result.Offset = min(a.Offset, b.Offset)

	// Handle unbounded locks
	if a.Length == 0 || b.Length == 0 {
		result.Length = 0 // Result is unbounded
		return result
	}

	// Both bounded - end is the maximum
	maxEnd := max(a.End(), b.End())

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
//
// PRECONDITION — whole-lock upgrade only. Step 3 flips the ENTIRE matched
// shared lock to exclusive, not just the [offset,length) sub-range, and that
// whole-range promotion is what gets persisted. This is correct ONLY because
// every caller upgrades a lock at exactly the range it was granted at: the
// NFSv4 LOCK upgrade path promotes the lock-owner's existing lock as a whole,
// never a strict sub-range of a larger shared lock. If a caller ever passes a
// sub-range of a wider shared lock, the surrounding bytes would be silently
// promoted to exclusive (and persisted that way) — split the matched lock into
// the upgraded sub-range plus shared remainder(s) before changing this. There
// are currently no production callers (NFSv4/NLM upgrade is not yet wired); the
// invariant is enforced by TestUpgradeLock_WholeLockOnly.
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

	// Persist the upgraded type under lm.mu so the change survives a restart.
	// Without this the in-memory lock reverted to shared on restart and a
	// reader could be wrongly granted against an intended-exclusive lock (R3-3).
	lm.persistUnifiedLockLocked(unifiedLocks[ownLockIndex])

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

	// Cross-protocol: an overlapping SMB byte-range lock must also block this
	// NLM/NFSv4 lock (area-5 H-3 / xproto H1). Skip for whole-file leases and
	// delegations, which are not byte-range locks.
	if !lock.IsLease() && !lock.IsDelegation() {
		smbLocks := lm.locks[handleKey]
		for i := range smbLocks {
			if fileLockConflictsWithUnified(&smbLocks[i], lock) {
				return NewLockConflictError("", unifiedConflictFromFileLock(&smbLocks[i]))
			}
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
			lm.persistUnifiedLockLocked(existing[i])
			return nil
		}
	}

	// Set acquisition time if not set
	if lock.AcquiredAt.IsZero() {
		lock.AcquiredAt = time.Now()
	}

	// Add new lock
	lm.unifiedLocks[handleKey] = append(existing, lock)
	lm.indexAddLockLocked(handleKey, lock)
	lm.persistUnifiedLockLocked(lock)
	return nil
}

// TestUnifiedLock previews whether a prospective NLM/NFSv4 byte-range lock would
// conflict, checking BOTH the unified lock map and the SMB byte-range map
// (lm.locks). Returns the conflicting lock, or nil if grantable. This keeps the
// NLM/NFSv4 TEST/LOCKT preview consistent with AddUnifiedLock enforcement: a
// preview that only scanned unifiedLocks would report a range grantable while
// an overlapping SMB byte-range lock in lm.locks would deny the acquire
// (area-5 H-3 / xproto H1).
func (lm *Manager) TestUnifiedLock(handleKey string, want *UnifiedLock) *UnifiedLockConflict {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	for _, el := range lm.unifiedLocks[handleKey] {
		if want.ConflictsWith(el) {
			return &UnifiedLockConflict{Lock: el, Reason: "lock conflict"}
		}
	}

	if !want.IsLease() && !want.IsDelegation() {
		smbLocks := lm.locks[handleKey]
		for i := range smbLocks {
			if fileLockConflictsWithUnified(&smbLocks[i], want) {
				return unifiedConflictFromFileLock(&smbLocks[i])
			}
		}
	}

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

		// Overlaps - split the lock. Delete the original record, then persist
		// each fragment (each carries a fresh UUID from SplitLock). Done under
		// lm.mu so the store sees delete-then-puts in mutation order.
		found = true
		lm.deleteUnifiedLockLocked(lock)
		splitResult := SplitLock(lock, offset, length)
		for _, frag := range splitResult {
			lm.persistUnifiedLockLocked(frag)
		}
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
	lm.reindexHandleLocked(handleKey, existing)

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

// RemoveFileUnifiedLocks removes all unified locks, delegations, and break
// wait channels for a file.
func (lm *Manager) RemoveFileUnifiedLocks(handleKey string) {
	lm.mu.Lock()
	old := lm.unifiedLocks[handleKey]
	delete(lm.unifiedLocks, handleKey)
	lm.reindexHandleLocked(handleKey, old)
	delete(lm.breakWaitChans, handleKey)
	lm.mu.Unlock()
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
// conflict with a write operation. Backward-compatible wrapper for CheckAndBreakCachingForWrite.
func (lm *Manager) CheckAndBreakOpLocksForWrite(handleKey string, excludeOwner *LockOwner) error {
	return lm.CheckAndBreakCachingForWrite(handleKey, excludeOwner)
}

// CheckAndBreakOpLocksForRead checks and initiates breaks for oplocks that
// conflict with a read operation. Backward-compatible wrapper for CheckAndBreakCachingForRead.
func (lm *Manager) CheckAndBreakOpLocksForRead(handleKey string, excludeOwner *LockOwner) error {
	return lm.CheckAndBreakCachingForRead(handleKey, excludeOwner)
}

// CheckAndBreakOpLocksForDelete checks and initiates breaks for all oplocks
// on a file being deleted. Backward-compatible wrapper for CheckAndBreakCachingForDelete.
func (lm *Manager) CheckAndBreakOpLocksForDelete(handleKey string, excludeOwner *LockOwner) error {
	return lm.CheckAndBreakCachingForDelete(handleKey, excludeOwner)
}
