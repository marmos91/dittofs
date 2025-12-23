package postgres

import (
	"context"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// ============================================================================
// Byte-Range File Locking (SMB/NLM support)
// ============================================================================

// byteRangeLockState holds locks for a single file.
type byteRangeLockState struct {
	mu    sync.RWMutex
	locks []metadata.FileLock
}

// byteRangeLockManager manages byte-range file locks.
// Locks are ephemeral (in-memory only) and lost on server restart.
// Each PostgresMetadataStore instance has its own lock manager to ensure
// lock isolation between different stores (e.g., different shares).
type byteRangeLockManager struct {
	locks sync.Map // handle string -> *byteRangeLockState
}

// getOrCreateState gets or creates lock state for a file handle.
func (m *byteRangeLockManager) getOrCreateState(handle string) *byteRangeLockState {
	state, _ := m.locks.LoadOrStore(handle, &byteRangeLockState{})
	return state.(*byteRangeLockState)
}

// lock attempts to acquire a byte-range lock on a file.
func (m *byteRangeLockManager) lock(handle string, lock metadata.FileLock) error {
	state := m.getOrCreateState(handle)
	state.mu.Lock()
	defer state.mu.Unlock()

	// Check for conflicts
	for i := range state.locks {
		if metadata.IsLockConflicting(&state.locks[i], &lock) {
			conflict := &metadata.LockConflict{
				Offset:         state.locks[i].Offset,
				Length:         state.locks[i].Length,
				Exclusive:      state.locks[i].Exclusive,
				OwnerSessionID: state.locks[i].SessionID,
			}
			return metadata.NewLockedError("", conflict)
		}
	}

	// Check if this exact lock already exists (same session, offset, length)
	for i := range state.locks {
		if state.locks[i].SessionID == lock.SessionID &&
			state.locks[i].Offset == lock.Offset &&
			state.locks[i].Length == lock.Length {
			// Update existing lock
			state.locks[i].Exclusive = lock.Exclusive
			state.locks[i].AcquiredAt = time.Now()
			state.locks[i].ID = lock.ID
			return nil
		}
	}

	// Set acquisition time if not set
	if lock.AcquiredAt.IsZero() {
		lock.AcquiredAt = time.Now()
	}

	// Add new lock
	state.locks = append(state.locks, lock)
	return nil
}

// unlock releases a specific byte-range lock.
func (m *byteRangeLockManager) unlock(handle string, sessionID, offset, length uint64) error {
	stateVal, ok := m.locks.Load(handle)
	if !ok {
		return metadata.NewLockNotFoundError("")
	}

	state := stateVal.(*byteRangeLockState)
	state.mu.Lock()
	defer state.mu.Unlock()

	for i := range state.locks {
		if state.locks[i].SessionID == sessionID &&
			state.locks[i].Offset == offset &&
			state.locks[i].Length == length {
			state.locks = append(state.locks[:i], state.locks[i+1:]...)
			return nil
		}
	}

	return metadata.NewLockNotFoundError("")
}

// unlockAllForSession releases all locks held by a session on a file.
func (m *byteRangeLockManager) unlockAllForSession(handle string, sessionID uint64) int {
	stateVal, ok := m.locks.Load(handle)
	if !ok {
		return 0
	}

	state := stateVal.(*byteRangeLockState)
	state.mu.Lock()
	defer state.mu.Unlock()

	remaining := make([]metadata.FileLock, 0, len(state.locks))
	removed := 0
	for i := range state.locks {
		if state.locks[i].SessionID == sessionID {
			removed++
		} else {
			remaining = append(remaining, state.locks[i])
		}
	}

	state.locks = remaining
	return removed
}

// testLock checks if a lock would succeed without acquiring it.
func (m *byteRangeLockManager) testLock(handle string, sessionID, offset, length uint64, exclusive bool) (bool, *metadata.LockConflict) {
	stateVal, ok := m.locks.Load(handle)
	if !ok {
		return true, nil
	}

	state := stateVal.(*byteRangeLockState)
	state.mu.RLock()
	defer state.mu.RUnlock()

	testLock := &metadata.FileLock{
		SessionID: sessionID,
		Offset:    offset,
		Length:    length,
		Exclusive: exclusive,
	}

	for i := range state.locks {
		if metadata.IsLockConflicting(&state.locks[i], testLock) {
			return false, &metadata.LockConflict{
				Offset:         state.locks[i].Offset,
				Length:         state.locks[i].Length,
				Exclusive:      state.locks[i].Exclusive,
				OwnerSessionID: state.locks[i].SessionID,
			}
		}
	}

	return true, nil
}

// checkForIO checks if an I/O operation would conflict with existing locks.
func (m *byteRangeLockManager) checkForIO(handle string, sessionID, offset, length uint64, isWrite bool) *metadata.LockConflict {
	stateVal, ok := m.locks.Load(handle)
	if !ok {
		return nil
	}

	state := stateVal.(*byteRangeLockState)
	state.mu.RLock()
	defer state.mu.RUnlock()

	for i := range state.locks {
		if metadata.CheckIOConflict(&state.locks[i], sessionID, offset, length, isWrite) {
			return &metadata.LockConflict{
				Offset:         state.locks[i].Offset,
				Length:         state.locks[i].Length,
				Exclusive:      state.locks[i].Exclusive,
				OwnerSessionID: state.locks[i].SessionID,
			}
		}
	}

	return nil
}

// listLocks returns all active locks on a file.
func (m *byteRangeLockManager) listLocks(handle string) []metadata.FileLock {
	stateVal, ok := m.locks.Load(handle)
	if !ok {
		return nil
	}

	state := stateVal.(*byteRangeLockState)
	state.mu.RLock()
	defer state.mu.RUnlock()

	if len(state.locks) == 0 {
		return nil
	}

	result := make([]metadata.FileLock, len(state.locks))
	copy(result, state.locks)
	return result
}

// ============================================================================
// MetadataStore Interface Implementation
// ============================================================================

// LockFile acquires a byte-range lock on a file.
func (s *PostgresMetadataStore) LockFile(ctx *metadata.AuthContext, handle metadata.FileHandle, lock metadata.FileLock) error {
	if err := ctx.Context.Err(); err != nil {
		return err
	}

	// Verify file exists and is not a directory
	file, err := s.GetFile(ctx.Context, handle)
	if err != nil {
		return err
	}

	if file.Type == metadata.FileTypeDirectory {
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

	// Acquire the lock
	handleKey := string(handle)
	return s.byteRangeLocks.lock(handleKey, lock)
}

// UnlockFile releases a specific byte-range lock.
func (s *PostgresMetadataStore) UnlockFile(ctx context.Context, handle metadata.FileHandle, sessionID uint64, offset uint64, length uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Verify file exists
	_, err := s.GetFile(ctx, handle)
	if err != nil {
		return err
	}

	handleKey := string(handle)
	return s.byteRangeLocks.unlock(handleKey, sessionID, offset, length)
}

// UnlockAllForSession releases all locks held by a session on a file.
func (s *PostgresMetadataStore) UnlockAllForSession(ctx context.Context, handle metadata.FileHandle, sessionID uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	handleKey := string(handle)
	s.byteRangeLocks.unlockAllForSession(handleKey, sessionID)
	return nil
}

// TestLock checks whether a lock would succeed without acquiring it.
func (s *PostgresMetadataStore) TestLock(ctx context.Context, handle metadata.FileHandle, sessionID uint64, offset uint64, length uint64, exclusive bool) (bool, *metadata.LockConflict, error) {
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}

	// Verify file exists
	_, err := s.GetFile(ctx, handle)
	if err != nil {
		return false, nil, err
	}

	handleKey := string(handle)
	ok, conflict := s.byteRangeLocks.testLock(handleKey, sessionID, offset, length, exclusive)
	return ok, conflict, nil
}

// CheckLockForIO verifies no conflicting locks exist for a read/write operation.
func (s *PostgresMetadataStore) CheckLockForIO(ctx context.Context, handle metadata.FileHandle, sessionID uint64, offset uint64, length uint64, isWrite bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	handleKey := string(handle)
	conflict := s.byteRangeLocks.checkForIO(handleKey, sessionID, offset, length, isWrite)
	if conflict != nil {
		return metadata.NewLockedError("", conflict)
	}
	return nil
}

// ListLocks returns all active locks on a file.
func (s *PostgresMetadataStore) ListLocks(ctx context.Context, handle metadata.FileHandle) ([]metadata.FileLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Verify file exists
	_, err := s.GetFile(ctx, handle)
	if err != nil {
		return nil, err
	}

	handleKey := string(handle)
	locks := s.byteRangeLocks.listLocks(handleKey)
	if locks == nil {
		return []metadata.FileLock{}, nil
	}
	return locks, nil
}
