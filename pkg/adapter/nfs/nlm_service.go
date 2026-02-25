package nfs

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// FileChecker provides file existence checking without importing pkg/metadata.
// This avoids an import cycle between the NFS adapter and the metadata package.
type FileChecker interface {
	// GetFile checks if a file exists and returns its type.
	// Returns exists=true if the file exists, isDir=true if it's a directory.
	GetFile(ctx context.Context, handle []byte) (exists bool, isDir bool, err error)
}

// NLMService provides NLM-specific lock operations using LockManager directly.
//
// This replaces the NLM methods that were previously on MetadataService,
// avoiding protocol-specific code in the generic metadata layer.
//
// The NLMService holds a reference to the lock.Manager for the relevant share
// and a FileChecker for validating file existence before lock operations.
//
// Thread Safety: Safe for concurrent use (delegates to thread-safe Manager).
type NLMService struct {
	lockMgr     *lock.Manager
	fileChecker FileChecker
	onUnlock    func(handle []byte) // callback for async unlock notification
}

// NewNLMService creates a new NLMService with the given lock manager and file checker.
func NewNLMService(lockMgr *lock.Manager, fileChecker FileChecker) *NLMService {
	return &NLMService{
		lockMgr:     lockMgr,
		fileChecker: fileChecker,
	}
}

// lockTypeFromExclusive converts an exclusive flag to a LockType.
func lockTypeFromExclusive(exclusive bool) lock.LockType {
	if exclusive {
		return lock.LockTypeExclusive
	}
	return lock.LockTypeShared
}

// checkFileExists verifies a file exists via the file checker.
// Returns nil on success, or a StoreError with ErrNotFound if the file does not exist.
func (s *NLMService) checkFileExists(ctx context.Context, handle []byte) error {
	exists, _, err := s.fileChecker.GetFile(ctx, handle)
	if err != nil {
		return err
	}
	if !exists {
		return &errors.StoreError{
			Code:    errors.ErrNotFound,
			Message: "file not found",
			Path:    string(handle),
		}
	}
	return nil
}

// SetUnlockCallback sets a callback invoked after each NLM unlock.
//
// The NLM blocking queue uses this to process waiting locks when a lock
// is released. The callback is called with the file handle of the unlocked file.
func (s *NLMService) SetUnlockCallback(fn func(handle []byte)) {
	s.onUnlock = fn
}

// LockFileNLM acquires a lock for NLM protocol.
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: File handle to lock (raw bytes)
//   - owner: Lock owner (contains protocol-prefixed ownerID)
//   - offset: Starting byte offset of lock range
//   - length: Number of bytes to lock (0 = to EOF)
//   - exclusive: true for exclusive/write lock, false for shared/read lock
//   - reclaim: true if this is a reclaim during grace period
//
// Returns:
//   - *lock.LockResult: Success=true with Lock if granted, Success=false with Conflict if denied
//   - error: System-level errors only (not lock conflicts)
func (s *NLMService) LockFileNLM(
	ctx context.Context,
	handle []byte,
	owner lock.LockOwner,
	offset, length uint64,
	exclusive bool,
	reclaim bool,
) (*lock.LockResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := s.checkFileExists(ctx, handle); err != nil {
		return nil, err
	}

	unifiedLock := lock.NewUnifiedLock(owner, lock.FileHandle(handle), offset, length, lockTypeFromExclusive(exclusive))
	unifiedLock.Reclaim = reclaim

	handleKey := string(handle)
	err := s.lockMgr.AddUnifiedLock(handleKey, unifiedLock)
	if err == nil {
		return &lock.LockResult{
			Success: true,
			Lock:    unifiedLock,
		}, nil
	}

	// Non-conflict errors are system failures
	storeErr, ok := err.(*errors.StoreError)
	if !ok || storeErr.Code != errors.ErrLockConflict {
		return nil, err
	}

	// Lock conflict: find the specific conflicting lock for the NLM response
	existing := s.lockMgr.ListUnifiedLocks(handleKey)
	for _, el := range existing {
		if lock.IsUnifiedLockConflicting(el, unifiedLock) {
			return &lock.LockResult{
				Success:  false,
				Conflict: &lock.UnifiedLockConflict{Lock: el, Reason: "conflict"},
			}, nil
		}
	}

	// Conflict detected but specific lock not found (race) - still return failure
	return &lock.LockResult{Success: false}, nil
}

// TestLockNLM tests if a lock could be granted without acquiring it.
//
// This is used for NLM_TEST procedure (F_GETLK fcntl() support).
// Per Phase 1 decision: TEST is allowed during grace period.
//
// Returns:
//   - bool: true if lock would succeed, false if conflict exists
//   - *lock.UnifiedLockConflict: Information about conflicting lock (nil if granted)
//   - error: System-level errors only
func (s *NLMService) TestLockNLM(
	ctx context.Context,
	handle []byte,
	owner lock.LockOwner,
	offset, length uint64,
	exclusive bool,
) (bool, *lock.UnifiedLockConflict, error) {
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}

	if err := s.checkFileExists(ctx, handle); err != nil {
		return false, nil, err
	}

	testLock := lock.NewUnifiedLock(owner, lock.FileHandle(handle), offset, length, lockTypeFromExclusive(exclusive))

	handleKey := string(handle)
	existing := s.lockMgr.ListUnifiedLocks(handleKey)
	for _, el := range existing {
		if lock.IsUnifiedLockConflicting(el, testLock) {
			return false, &lock.UnifiedLockConflict{Lock: el, Reason: "conflict"}, nil
		}
	}
	return true, nil, nil
}

// UnlockFileNLM releases a lock for NLM protocol.
//
// Per NLM specification:
//   - Unlock of non-existent lock silently succeeds (returns nil)
//   - This ensures idempotency for retried unlock requests
//
// After a successful unlock, the unlock callback is invoked (if set)
// to allow the blocking queue to process waiting lock requests.
func (s *NLMService) UnlockFileNLM(
	ctx context.Context,
	handle []byte,
	ownerID string,
	offset, length uint64,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	handleKey := string(handle)
	err := s.lockMgr.RemoveUnifiedLock(handleKey, lock.LockOwner{OwnerID: ownerID}, offset, length)
	if err != nil {
		// Per NLM spec: unlock of non-existent lock silently succeeds
		if storeErr, ok := err.(*errors.StoreError); ok && storeErr.Code == errors.ErrLockNotFound {
			return nil
		}
		return err
	}

	// Notify NLM blocking queue that a lock was released
	if s.onUnlock != nil {
		s.onUnlock(handle)
	}

	return nil
}

// CancelBlockingLock cancels a pending blocking lock request.
//
// This is used for NLM_CANCEL procedure when a client times out waiting
// for a blocked lock request. Currently a stub (blocking queue handles cancellation).
func (s *NLMService) CancelBlockingLock(
	ctx context.Context,
	handle []byte,
	ownerID string,
	offset, length uint64,
) error {
	// Stub - blocking queue handles cancellation directly
	return nil
}
