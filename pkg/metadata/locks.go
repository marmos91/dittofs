package metadata

import "time"

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
// Business Logic Functions for File Locking
// ============================================================================
//
// These functions implement file locking operations with proper business logic:
// - Permission checking
// - File type validation
// - Error handling
//
// Locks are managed by LockManager (one per share) at the MetadataService level.
// The store is only used for file existence checks and permission verification.

// lockFileWithManager acquires a byte-range lock on a file.
//
// Business logic:
//   - Verifies file exists
//   - Verifies file is not a directory (directories cannot be locked)
//   - Checks user has appropriate permission (read for shared, write for exclusive)
//
// Returns:
//   - error: ErrLocked if conflict exists, ErrNotFound if file doesn't exist,
//     ErrIsDirectory if target is a directory, ErrPermissionDenied if no permission
func lockFileWithManager(store MetadataStore, lm *LockManager, ctx *AuthContext, handle FileHandle, lock FileLock) error {
	if err := ctx.Context.Err(); err != nil {
		return err
	}

	// Verify file exists and is not a directory
	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return err
	}

	if file.Type == FileTypeDirectory {
		return NewIsDirectoryError("")
	}

	// Check permissions
	var requiredPerm Permission
	if lock.Exclusive {
		requiredPerm = PermissionWrite
	} else {
		requiredPerm = PermissionRead
	}

	granted, err := CheckFilePermissions(store, ctx, handle, requiredPerm)
	if err != nil {
		return err
	}
	if granted&requiredPerm == 0 {
		return NewPermissionDeniedError("")
	}

	// Acquire the lock via LockManager
	handleKey := string(handle)
	return lm.Lock(handleKey, lock)
}

// testFileLockWithManager checks whether a lock would succeed without acquiring it.
//
// Business logic:
//   - Verifies file exists
//
// Returns:
//   - bool: true if lock would succeed, false if conflict exists
//   - *LockConflict: Details of conflicting lock if bool is false
//   - error: ErrNotFound if file doesn't exist
func testFileLockWithManager(store MetadataStore, lm *LockManager, ctx *AuthContext, handle FileHandle, sessionID, offset, length uint64, exclusive bool) (bool, *LockConflict, error) {
	if err := ctx.Context.Err(); err != nil {
		return false, nil, err
	}

	// Verify file exists
	_, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return false, nil, err
	}

	handleKey := string(handle)
	ok, conflict := lm.TestLock(handleKey, sessionID, offset, length, exclusive)
	return ok, conflict, nil
}

// listFileLocksWithManager returns all active locks on a file.
//
// Business logic:
//   - Verifies file exists
//
// Returns:
//   - []FileLock: All active locks on the file (empty slice if none)
//   - error: ErrNotFound if file doesn't exist
func listFileLocksWithManager(store MetadataStore, lm *LockManager, ctx *AuthContext, handle FileHandle) ([]FileLock, error) {
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Verify file exists
	_, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return nil, err
	}

	handleKey := string(handle)
	locks := lm.ListLocks(handleKey)
	if locks == nil {
		return []FileLock{}, nil
	}
	return locks, nil
}
