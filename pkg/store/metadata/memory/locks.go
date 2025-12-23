package memory

import (
	"context"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// lockManager manages byte-range locks for the memory metadata store.
//
// This is an in-memory implementation that tracks locks per file handle.
// Locks are ephemeral and lost on server restart.
type lockManager struct {
	mu    sync.RWMutex
	locks map[string][]metadata.FileLock // handle string -> locks
}

// newLockManager creates a new lock manager.
func newLockManager() *lockManager {
	return &lockManager{
		locks: make(map[string][]metadata.FileLock),
	}
}

// lock attempts to acquire a byte-range lock on a file.
//
// Returns nil on success, or an error if a conflict exists.
func (lm *lockManager) lock(handle string, lock metadata.FileLock) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	existing := lm.locks[handle]

	// Check for conflicts with existing locks
	for i := range existing {
		if metadata.IsLockConflicting(&existing[i], &lock) {
			conflict := &metadata.LockConflict{
				Offset:         existing[i].Offset,
				Length:         existing[i].Length,
				Exclusive:      existing[i].Exclusive,
				OwnerSessionID: existing[i].SessionID,
			}
			return metadata.NewLockedError("", conflict)
		}
	}

	// Check if this exact lock already exists (same session, offset, length)
	// If so, update it (allows changing exclusive flag)
	for i := range existing {
		if existing[i].SessionID == lock.SessionID &&
			existing[i].Offset == lock.Offset &&
			existing[i].Length == lock.Length {
			// Update existing lock
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
	lm.locks[handle] = append(existing, lock)
	return nil
}

// unlock releases a specific byte-range lock.
//
// Returns nil on success, or an error if the lock wasn't found.
func (lm *lockManager) unlock(handle string, sessionID, offset, length uint64) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	existing := lm.locks[handle]
	if len(existing) == 0 {
		return metadata.NewLockNotFoundError("")
	}

	// Find and remove the matching lock
	for i := range existing {
		if existing[i].SessionID == sessionID &&
			existing[i].Offset == offset &&
			existing[i].Length == length {
			// Remove this lock
			lm.locks[handle] = append(existing[:i], existing[i+1:]...)

			// Clean up empty entries
			if len(lm.locks[handle]) == 0 {
				delete(lm.locks, handle)
			}
			return nil
		}
	}

	return metadata.NewLockNotFoundError("")
}

// unlockAllForSession releases all locks held by a session on a file.
//
// Returns the number of locks released.
func (lm *lockManager) unlockAllForSession(handle string, sessionID uint64) int {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	existing := lm.locks[handle]
	if len(existing) == 0 {
		return 0
	}

	// Filter out locks belonging to this session
	remaining := make([]metadata.FileLock, 0, len(existing))
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
		delete(lm.locks, handle)
	} else {
		lm.locks[handle] = remaining
	}

	return removed
}

// testLock checks if a lock would succeed without acquiring it.
//
// Returns (true, nil) if lock would succeed, (false, conflict) if conflict exists.
func (lm *lockManager) testLock(handle string, sessionID, offset, length uint64, exclusive bool) (bool, *metadata.LockConflict) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	existing := lm.locks[handle]

	// Create a test lock to check conflicts
	testLock := &metadata.FileLock{
		SessionID: sessionID,
		Offset:    offset,
		Length:    length,
		Exclusive: exclusive,
	}

	for i := range existing {
		if metadata.IsLockConflicting(&existing[i], testLock) {
			return false, &metadata.LockConflict{
				Offset:         existing[i].Offset,
				Length:         existing[i].Length,
				Exclusive:      existing[i].Exclusive,
				OwnerSessionID: existing[i].SessionID,
			}
		}
	}

	return true, nil
}

// checkForIO checks if an I/O operation would conflict with existing locks.
//
// Returns nil if I/O is allowed, or conflict details if blocked.
func (lm *lockManager) checkForIO(handle string, sessionID, offset, length uint64, isWrite bool) *metadata.LockConflict {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	existing := lm.locks[handle]

	for i := range existing {
		if metadata.CheckIOConflict(&existing[i], sessionID, offset, length, isWrite) {
			return &metadata.LockConflict{
				Offset:         existing[i].Offset,
				Length:         existing[i].Length,
				Exclusive:      existing[i].Exclusive,
				OwnerSessionID: existing[i].SessionID,
			}
		}
	}

	return nil
}

// listLocks returns all active locks on a file.
func (lm *lockManager) listLocks(handle string) []metadata.FileLock {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	existing := lm.locks[handle]
	if len(existing) == 0 {
		return nil
	}

	// Return a copy to avoid race conditions
	result := make([]metadata.FileLock, len(existing))
	copy(result, existing)
	return result
}

// removeFile removes all locks for a file (called when file is deleted).
func (lm *lockManager) removeFile(handle string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	delete(lm.locks, handle)
}

// ============================================================================
// MetadataStore Interface Implementation
// ============================================================================

// LockFile acquires a byte-range lock on a file.
func (s *MemoryMetadataStore) LockFile(ctx *metadata.AuthContext, handle metadata.FileHandle, lock metadata.FileLock) error {
	if err := ctx.Context.Err(); err != nil {
		return err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Verify file exists and is not a directory
	handleKey := handleToKey(handle)
	file, ok := s.files[handleKey]
	if !ok {
		return metadata.NewNotFoundError("", "file")
	}

	if file.Attr.Type == metadata.FileTypeDirectory {
		return metadata.NewIsDirectoryError("")
	}

	// Check permissions
	var requiredPerm metadata.Permission
	if lock.Exclusive {
		requiredPerm = metadata.PermissionWrite
	} else {
		requiredPerm = metadata.PermissionRead
	}

	granted, err := s.CheckPermissions(ctx, handle, requiredPerm)
	if err != nil {
		return err
	}
	if granted&requiredPerm == 0 {
		return metadata.NewPermissionDeniedError("")
	}

	// Acquire the lock (lockMgr has its own internal mutex)
	return s.lockMgr.lock(handleKey, lock)
}

// UnlockFile releases a specific byte-range lock.
func (s *MemoryMetadataStore) UnlockFile(ctx context.Context, handle metadata.FileHandle, sessionID uint64, offset uint64, length uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Verify file exists
	handleKey := handleToKey(handle)
	if _, ok := s.files[handleKey]; !ok {
		return metadata.NewNotFoundError("", "file")
	}

	return s.lockMgr.unlock(handleKey, sessionID, offset, length)
}

// UnlockAllForSession releases all locks held by a session on a file.
func (s *MemoryMetadataStore) UnlockAllForSession(ctx context.Context, handle metadata.FileHandle, sessionID uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Note: We don't check if file exists - it may have been deleted
	// and we still want to clean up any stale lock entries
	handleKey := handleToKey(handle)
	s.lockMgr.unlockAllForSession(handleKey, sessionID)
	return nil
}

// TestLock checks whether a lock would succeed without acquiring it.
func (s *MemoryMetadataStore) TestLock(ctx context.Context, handle metadata.FileHandle, offset uint64, length uint64, exclusive bool) (bool, *metadata.LockConflict, error) {
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Verify file exists
	handleKey := handleToKey(handle)
	if _, ok := s.files[handleKey]; !ok {
		return false, nil, metadata.NewNotFoundError("", "file")
	}

	// Use sessionID 0 to test as a hypothetical new session
	// This will detect conflicts with any existing locks
	ok, conflict := s.lockMgr.testLock(handleKey, 0, offset, length, exclusive)
	return ok, conflict, nil
}

// CheckLockForIO verifies no conflicting locks exist for a read/write operation.
func (s *MemoryMetadataStore) CheckLockForIO(ctx context.Context, handle metadata.FileHandle, sessionID uint64, offset uint64, length uint64, isWrite bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Note: We don't check if file exists here since the caller should have
	// already verified file existence. This allows fast path for I/O operations.
	handleKey := handleToKey(handle)
	conflict := s.lockMgr.checkForIO(handleKey, sessionID, offset, length, isWrite)
	if conflict != nil {
		return metadata.NewLockedError("", conflict)
	}
	return nil
}

// ListLocks returns all active locks on a file.
func (s *MemoryMetadataStore) ListLocks(ctx context.Context, handle metadata.FileHandle) ([]metadata.FileLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Verify file exists
	handleKey := handleToKey(handle)
	if _, ok := s.files[handleKey]; !ok {
		return nil, metadata.NewNotFoundError("", "file")
	}

	locks := s.lockMgr.listLocks(handleKey)
	if locks == nil {
		return []metadata.FileLock{}, nil
	}
	return locks, nil
}
