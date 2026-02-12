package metadata

import (
	"context"
	stderrors "errors"
	"fmt"
	"sync"

	"github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// MetadataService provides all metadata operations for the filesystem.
//
// It manages metadata stores and routes operations to the correct store
// based on share name. All protocol handlers should interact with MetadataService
// rather than accessing stores directly.
//
// File Locking:
// MetadataService owns one LockManager per share for byte-range locking (SMB/NLM).
// Locks are ephemeral (in-memory only) and lost on server restart.
// This is separate from metadata stores which handle persistent data.
//
// Usage:
//
//	metaSvc := metadata.New()
//	metaSvc.RegisterStoreForShare("/export", memoryStore)
//	metaSvc.RegisterStoreForShare("/archive", badgerStore)
//
//	// High-level operations (with business logic)
//	file, err := metaSvc.CreateFile(authCtx, parentHandle, "test.txt", fileAttr)
//
//	// Low-level operations (direct store access)
//	file, err := metaSvc.GetFile(ctx, handle)
type MetadataService struct {
	mu               sync.RWMutex
	stores           map[string]MetadataStore    // shareName -> store
	lockManagers     map[string]*LockManager     // shareName -> lock manager (ephemeral, per-share)
	unifiedViews     map[string]*UnifiedLockView // shareName -> unified lock view (cross-protocol)
	pendingWrites    *PendingWritesTracker       // deferred metadata commits for performance
	deferredCommit   bool                        // if true, use deferred commits (default: true)
	cookies          *CookieManager              // NFS/SMB cookie ↔ store token translation
	onUnlockCallback func(handle FileHandle)     // NLM callback to process waiting locks
}

// New creates a new empty MetadataService instance.
// Use RegisterStoreForShare to configure stores for each share.
// By default, deferred commits are enabled for better write performance.
func New() *MetadataService {
	return &MetadataService{
		stores:         make(map[string]MetadataStore),
		lockManagers:   make(map[string]*LockManager),
		unifiedViews:   make(map[string]*UnifiedLockView),
		pendingWrites:  NewPendingWritesTracker(),
		deferredCommit: true, // Enable deferred commits by default
		cookies:        NewCookieManager(),
	}
}

// SetDeferredCommit enables or disables deferred metadata commits.
// When enabled, CommitWrite batches updates until FlushPendingWrites is called.
// This significantly improves write performance for sequential workloads.
func (s *MetadataService) SetDeferredCommit(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deferredCommit = enabled
}

// RegisterStoreForShare associates a metadata store with a share.
// Each share must have exactly one store. Calling this again for the same
// share will replace the previous store.
//
// This also creates a LockManager for the share if one doesn't exist.
// Lock managers are ephemeral and not replaced when re-registering a store.
func (s *MetadataService) RegisterStoreForShare(shareName string, store MetadataStore) error {
	if store == nil {
		return fmt.Errorf("cannot register nil store for share %q", shareName)
	}
	if shareName == "" {
		return fmt.Errorf("cannot register store for empty share name")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.stores[shareName] = store

	// Create a lock manager for this share if it doesn't exist
	if _, exists := s.lockManagers[shareName]; !exists {
		s.lockManagers[shareName] = NewLockManager()
	}

	return nil
}

// GetStoreForShare returns the metadata store for a specific share.
// This is primarily for internal use and testing; protocol handlers
// should use the high-level methods instead.
func (s *MetadataService) GetStoreForShare(shareName string) (MetadataStore, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if store, ok := s.stores[shareName]; ok {
		return store, nil
	}

	return nil, fmt.Errorf("no store configured for share %q", shareName)
}

// storeForHandle returns the appropriate store for a file handle.
// It extracts the share name from the handle and looks up the store.
func (s *MetadataService) storeForHandle(handle FileHandle) (MetadataStore, error) {
	shareName, _, err := DecodeFileHandle(handle)
	if err != nil {
		return nil, fmt.Errorf("invalid file handle: %w", err)
	}

	return s.GetStoreForShare(shareName)
}

// lockManagerForHandle returns the lock manager for the share that owns the handle.
func (s *MetadataService) lockManagerForHandle(handle FileHandle) (*LockManager, error) {
	shareName, _, err := DecodeFileHandle(handle)
	if err != nil {
		return nil, fmt.Errorf("invalid file handle: %w", err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if lm, ok := s.lockManagers[shareName]; ok {
		return lm, nil
	}

	return nil, fmt.Errorf("no lock manager for share %q", shareName)
}

// GetLockManagerForShare returns the lock manager for a specific share.
//
// This is used by the NFS adapter to process NLM blocking lock waiters.
// Returns nil if no lock manager exists for the share.
//
// Thread safety: Safe to call concurrently.
func (s *MetadataService) GetLockManagerForShare(shareName string) *LockManager {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if lm, ok := s.lockManagers[shareName]; ok {
		return lm
	}
	return nil
}

// GetUnifiedLockView returns the UnifiedLockView for a specific share.
//
// UnifiedLockView provides cross-protocol lock visibility, allowing any protocol
// handler to query all locks (NLM byte-range and SMB leases) on a file.
//
// Returns nil if no UnifiedLockView exists for the share. This can happen if:
//   - The share has not been registered
//   - No LockStore has been set for the share
//
// Thread safety: Safe to call concurrently.
func (s *MetadataService) GetUnifiedLockView(shareName string) *UnifiedLockView {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if view, ok := s.unifiedViews[shareName]; ok {
		return view
	}
	return nil
}

// SetUnifiedLockView sets the UnifiedLockView for a specific share.
//
// This is called when a LockStore becomes available for a share (e.g., when
// a store that implements LockStore is registered). Protocol handlers should
// NOT call this directly - it's for internal use by the registration process.
//
// Thread safety: Safe to call concurrently.
func (s *MetadataService) SetUnifiedLockView(shareName string, view *UnifiedLockView) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.unifiedViews[shareName] = view
}

// ============================================================================
// Low-Level Store Operations
// ============================================================================
// These methods provide direct access to store operations without additional
// business logic. They route to the correct store based on the handle's share.

// GetFile retrieves file metadata by handle.
// This is a convenience method that calls GetFile from the Base interface.
// When deferred commits are enabled, it merges pending write state (size, mtime, ctime)
// with the stored file metadata.
func (s *MetadataService) GetFile(ctx context.Context, handle FileHandle) (*File, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		return nil, err
	}

	// Check for pending write state (when deferred commits are enabled)
	if pending, ok := s.pendingWrites.GetPending(handle); ok {
		// Merge pending state with stored state
		if pending.MaxSize > file.Size {
			file.Size = pending.MaxSize
		}
		// Update timestamps from pending state
		if pending.LastMtime.After(file.Mtime) {
			file.Mtime = pending.LastMtime
			file.Ctime = pending.LastMtime
		}
		// Apply setuid/setgid clearing
		if pending.ClearSetuidSetgid {
			file.Mode &= ^uint32(0o6000)
		}
	}

	return file, nil
}

// CheckPermissions performs file-level permission checking.
// Returns granted permissions (subset of requested).
//
// This implements Unix-style permission checking:
//   - Root (UID 0): Bypass all checks except on read-only shares
//   - Owner: Check owner permission bits
//   - Group member: Check group permission bits
//   - Other: Check other permission bits
//   - Anonymous: Only world permissions
func (s *MetadataService) CheckPermissions(ctx *AuthContext, handle FileHandle, requested Permission) (Permission, error) {
	return s.checkFilePermissions(ctx, handle, requested)
}

// GetChild retrieves a child's handle from a directory.
func (s *MetadataService) GetChild(ctx context.Context, dirHandle FileHandle, name string) (FileHandle, error) {
	store, err := s.storeForHandle(dirHandle)
	if err != nil {
		return nil, err
	}
	return store.GetChild(ctx, dirHandle, name)
}

// GetRootHandle returns the root handle for a share.
func (s *MetadataService) GetRootHandle(ctx context.Context, shareName string) (FileHandle, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, err
	}
	return store.GetRootHandle(ctx, shareName)
}

// GenerateHandle generates a new file handle for a path.
func (s *MetadataService) GenerateHandle(ctx context.Context, shareName, path string) (FileHandle, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, err
	}
	return store.GenerateHandle(ctx, shareName, path)
}

// GetFilesystemStatistics returns filesystem statistics.
func (s *MetadataService) GetFilesystemStatistics(ctx context.Context, handle FileHandle) (*FilesystemStatistics, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}
	return store.GetFilesystemStatistics(ctx, handle)
}

// GetFilesystemCapabilities returns filesystem capabilities.
func (s *MetadataService) GetFilesystemCapabilities(ctx context.Context, handle FileHandle) (*FilesystemCapabilities, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}
	return store.GetFilesystemCapabilities(ctx, handle)
}

// ============================================================================
// Locking Operations (for SMB/NLM)
// ============================================================================
//
// File locking is managed by LockManager instances (one per share).
// Locks are ephemeral (in-memory only) and lost on server restart.
// Business logic (permission checking, file type validation) is in locks.go.

// CheckLockForIO checks if an I/O operation is blocked by locks.
//
// This is a lightweight operation that doesn't verify file existence,
// allowing fast path for I/O operations.
func (s *MetadataService) CheckLockForIO(ctx context.Context, handle FileHandle, sessionID, offset, length uint64, isWrite bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return err
	}

	handleKey := string(handle)
	conflict := lm.CheckForIO(handleKey, sessionID, offset, length, isWrite)
	if conflict != nil {
		return NewLockedError("", conflict)
	}
	return nil
}

// LockFile acquires a byte-range lock on a file.
//
// Business logic:
//   - Verifies file exists
//   - Verifies file is not a directory (directories cannot be locked)
//   - Checks user has appropriate permission (read for shared, write for exclusive)
func (s *MetadataService) LockFile(ctx *AuthContext, handle FileHandle, lock FileLock) error {
	if err := ctx.Context.Err(); err != nil {
		return err
	}

	store, err := s.storeForHandle(handle)
	if err != nil {
		return err
	}

	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
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

	// Get share options for permission check
	shareOpts, err := store.GetShareOptions(ctx.Context, file.ShareName)
	if err != nil {
		shareOpts = nil // Continue without share options if unavailable
	}

	granted := calculatePermissions(file, ctx.Identity, shareOpts, requiredPerm)
	if granted&requiredPerm == 0 {
		return NewPermissionDeniedError("")
	}

	// Acquire the lock via LockManager
	handleKey := string(handle)
	return lm.Lock(handleKey, lock)
}

// UnlockFile releases a byte-range lock on a file.
//
// Note: Takes context.Context instead of *AuthContext because:
// - Session ID identifies the lock owner (you can only unlock your own locks)
// - No permission checking needed for unlock operations
func (s *MetadataService) UnlockFile(ctx context.Context, handle FileHandle, sessionID, offset, length uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	store, err := s.storeForHandle(handle)
	if err != nil {
		return err
	}

	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return err
	}

	// Verify file exists
	_, err = store.GetFile(ctx, handle)
	if err != nil {
		return err
	}

	handleKey := string(handle)
	return lm.Unlock(handleKey, sessionID, offset, length)
}

// UnlockAllForSession releases all locks held by a session on a file.
func (s *MetadataService) UnlockAllForSession(ctx context.Context, handle FileHandle, sessionID uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return err
	}

	// No file existence check - file may have been deleted
	handleKey := string(handle)
	lm.UnlockAllForSession(handleKey, sessionID)
	return nil
}

// TestLock tests if a lock would conflict with existing locks.
//
// Business logic:
//   - Verifies file exists
//
// Returns:
//   - bool: true if lock would succeed, false if conflict exists
//   - *LockConflict: Details of conflicting lock if bool is false
func (s *MetadataService) TestLock(ctx *AuthContext, handle FileHandle, sessionID, offset, length uint64, exclusive bool) (bool, *LockConflict, error) {
	if err := ctx.Context.Err(); err != nil {
		return false, nil, err
	}

	store, err := s.storeForHandle(handle)
	if err != nil {
		return false, nil, err
	}

	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return false, nil, err
	}

	// Verify file exists
	_, err = store.GetFile(ctx.Context, handle)
	if err != nil {
		return false, nil, err
	}

	handleKey := string(handle)
	ok, conflict := lm.TestLock(handleKey, sessionID, offset, length, exclusive)
	return ok, conflict, nil
}

// ListLocks lists all locks on a file.
//
// Business logic:
//   - Verifies file exists
//
// Returns:
//   - []FileLock: All active locks on the file (empty slice if none)
func (s *MetadataService) ListLocks(ctx *AuthContext, handle FileHandle) ([]FileLock, error) {
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}

	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return nil, err
	}

	// Verify file exists
	_, err = store.GetFile(ctx.Context, handle)
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

// RemoveFileLocks removes all locks for a file.
// Called when a file is deleted to clean up stale lock entries.
func (s *MetadataService) RemoveFileLocks(handle FileHandle) {
	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return // No lock manager means no locks to remove
	}

	handleKey := string(handle)
	lm.RemoveFileLocks(handleKey)
}

// ============================================================================
// Share Management
// ============================================================================

// CreateShare creates a new share with its root directory.
func (s *MetadataService) CreateShare(ctx context.Context, shareName string, share *Share) error {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return err
	}
	return store.CreateShare(ctx, share)
}

// GetShareOptions returns the options for a share.
func (s *MetadataService) GetShareOptions(ctx context.Context, shareName string) (*ShareOptions, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, err
	}
	return store.GetShareOptions(ctx, shareName)
}

// ============================================================================
// NLM Lock Operations
// ============================================================================
//
// These methods provide NLM-specific lock operations that integrate with the
// unified lock manager from Phase 1. They differ from the generic lock methods
// in that they:
//   - Take owner ID string directly (NLM handler constructs it)
//   - Return detailed conflict info for NLM_DENIED responses
//   - Support blocking semantics via lock.LockResult
//
// Owner ID format per CONTEXT.md: nlm:{caller_name}:{svid}:{oh_hex}

// LockFileNLM acquires a lock for NLM protocol.
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: File handle to lock
//   - owner: Lock owner (contains protocol-prefixed ownerID)
//   - offset: Starting byte offset of lock range
//   - length: Number of bytes to lock (0 = to EOF)
//   - exclusive: true for exclusive/write lock, false for shared/read lock
//   - reclaim: true if this is a reclaim during grace period
//
// Returns:
//   - *lock.LockResult: Success=true with Lock if granted, Success=false with Conflict if denied
//   - error: System-level errors only (not lock conflicts)
func (s *MetadataService) LockFileNLM(
	ctx context.Context,
	handle FileHandle,
	owner LockOwner,
	offset, length uint64,
	exclusive bool,
	reclaim bool,
) (*lock.LockResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Get lock manager for the handle's share
	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return nil, err
	}

	// Verify file exists
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}
	_, err = store.GetFile(ctx, handle)
	if err != nil {
		return nil, err // Will map to NLM4_STALE_FH
	}

	// Create enhanced lock
	lockType := LockTypeShared
	if exclusive {
		lockType = LockTypeExclusive
	}
	enhancedLock := NewEnhancedLock(owner, lock.FileHandle(handle), offset, length, lockType)
	enhancedLock.Reclaim = reclaim

	// Try to acquire
	handleKey := string(handle)
	err = lm.AddEnhancedLock(handleKey, enhancedLock)
	if err != nil {
		// Check if it's a lock conflict error (StoreError with ErrLockConflict code)
		if storeErr, ok := err.(*errors.StoreError); ok && storeErr.Code == errors.ErrLockConflict {
			// For NLM, we need to find the conflicting lock for the response
			existing := lm.ListEnhancedLocks(handleKey)
			for _, el := range existing {
				if IsEnhancedLockConflicting(el, enhancedLock) {
					return &lock.LockResult{
						Success:  false,
						Conflict: &EnhancedLockConflict{Lock: el, Reason: "conflict"},
					}, nil
				}
			}
			// Conflict but couldn't find specific lock - still return failure
			return &lock.LockResult{
				Success: false,
			}, nil
		}
		return nil, err
	}

	return &lock.LockResult{
		Success: true,
		Lock:    enhancedLock,
	}, nil
}

// TestLockNLM tests if a lock could be granted without acquiring it.
//
// This is used for NLM_TEST procedure (F_GETLK fcntl() support).
// Per Phase 1 decision: TEST is allowed during grace period.
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: File handle to test
//   - owner: Lock owner for the test
//   - offset: Starting byte offset of test range
//   - length: Number of bytes to test (0 = to EOF)
//   - exclusive: true to test for exclusive lock, false for shared lock
//
// Returns:
//   - bool: true if lock would succeed, false if conflict exists
//   - *EnhancedLockConflict: Information about conflicting lock (nil if granted)
//   - error: System-level errors only
func (s *MetadataService) TestLockNLM(
	ctx context.Context,
	handle FileHandle,
	owner LockOwner,
	offset, length uint64,
	exclusive bool,
) (bool, *EnhancedLockConflict, error) {
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}

	// Get lock manager
	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return false, nil, err
	}

	// Verify file exists
	store, err := s.storeForHandle(handle)
	if err != nil {
		return false, nil, err
	}
	_, err = store.GetFile(ctx, handle)
	if err != nil {
		return false, nil, err
	}

	// Test the lock
	lockType := LockTypeShared
	if exclusive {
		lockType = LockTypeExclusive
	}
	testLock := NewEnhancedLock(owner, lock.FileHandle(handle), offset, length, lockType)

	handleKey := string(handle)
	existing := lm.ListEnhancedLocks(handleKey)
	for _, el := range existing {
		if IsEnhancedLockConflicting(el, testLock) {
			return false, &EnhancedLockConflict{Lock: el, Reason: "conflict"}, nil
		}
	}
	return true, nil, nil
}

// SetNLMUnlockCallback sets a callback invoked after each NLM unlock.
//
// The NLM blocking queue uses this to process waiting locks when a lock
// is released. The callback is called asynchronously (in a goroutine by
// the caller) to avoid blocking the unlock operation.
//
// Parameters:
//   - fn: Callback function that receives the file handle of the unlocked file
func (s *MetadataService) SetNLMUnlockCallback(fn func(handle FileHandle)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onUnlockCallback = fn
}

// UnlockFileNLM releases a lock for NLM protocol.
//
// Per NLM specification and CONTEXT.md:
//   - Unlock of non-existent lock silently succeeds (returns nil)
//   - This ensures idempotency for retried unlock requests
//   - The exclusive flag is ignored on unlock
//
// After a successful unlock, the NLM unlock callback is invoked (if set)
// to allow the blocking queue to process waiting lock requests.
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: File handle to unlock
//   - ownerID: Owner ID string (protocol-prefixed)
//   - offset: Starting byte offset of lock range
//   - length: Number of bytes to unlock (0 = to EOF)
//
// Returns:
//   - error: nil on success (including non-existent lock), system errors otherwise
func (s *MetadataService) UnlockFileNLM(
	ctx context.Context,
	handle FileHandle,
	ownerID string,
	offset, length uint64,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return nil // No lock manager = no locks = success
	}

	handleKey := string(handle)
	err = lm.RemoveEnhancedLock(handleKey, LockOwner{OwnerID: ownerID}, offset, length)
	if err != nil {
		// Per CONTEXT.md: unlock of non-existent lock silently succeeds
		if storeErr, ok := err.(*errors.StoreError); ok && storeErr.Code == errors.ErrLockNotFound {
			return nil
		}
		return err
	}

	// Notify NLM blocking queue that a lock was released
	// Read callback under lock, call outside lock to avoid deadlock
	s.mu.RLock()
	callback := s.onUnlockCallback
	s.mu.RUnlock()
	if callback != nil {
		callback(handle)
	}

	return nil
}

// CancelBlockingLock cancels a pending blocking lock request.
//
// This is used for NLM_CANCEL procedure when a client times out waiting
// for a blocked lock request.
//
// Note: This is a stub for now. Full implementation will be added in Plan 02-03
// when the blocking queue is implemented.
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: File handle
//   - ownerID: Owner ID of the pending request
//   - offset: Starting byte offset of the pending lock
//   - length: Number of bytes of the pending lock
//
// Returns:
//   - error: nil on success, system errors otherwise
func (s *MetadataService) CancelBlockingLock(
	ctx context.Context,
	handle FileHandle,
	ownerID string,
	offset, length uint64,
) error {
	// Stub for now - blocking queue implementation in Plan 02-03
	// For now, just return success since we don't have a blocking queue
	return nil
}

// ============================================================================
// Cross-Protocol Lease Integration (SMB Leases)
// ============================================================================
//
// These methods provide NFS protocol handlers with visibility into SMB leases.
// When an NFS operation conflicts with an SMB lease, the lease must be broken
// before the NFS operation can proceed.
//
// The OplockManager interface is injected by the SMB adapter when it starts.
// If no SMB adapter is running, these methods are no-ops.

// ErrLeaseBreakPending indicates that a lease break is in progress and the
// caller should wait for acknowledgment before proceeding with the operation.
// Returned by OplockChecker methods when an SMB client has a Write lease that
// must be broken before an NFS/NLM operation can complete.
var ErrLeaseBreakPending = stderrors.New("lease break pending, operation must wait")

// OplockChecker defines the interface for cross-protocol lease checking.
// This interface is implemented by the SMB OplockManager.
type OplockChecker interface {
	// CheckAndBreakForWrite checks for SMB leases that conflict with a write
	// and initiates breaks as needed. Returns ErrLeaseBreakPending if caller
	// should wait for break acknowledgment.
	CheckAndBreakForWrite(ctx context.Context, fileHandle lock.FileHandle) error

	// CheckAndBreakForRead checks for SMB leases that conflict with a read
	// and initiates breaks as needed. Returns ErrLeaseBreakPending if caller
	// should wait for break acknowledgment.
	CheckAndBreakForRead(ctx context.Context, fileHandle lock.FileHandle) error

	// CheckAndBreakForDelete checks for SMB Handle leases that must break
	// before file deletion. H leases protect against surprise deletion.
	// Returns ErrLeaseBreakPending if caller should wait for break acknowledgment.
	CheckAndBreakForDelete(ctx context.Context, fileHandle lock.FileHandle) error
}

// oplockChecker is the optional cross-protocol lease checker.
// Set by SMB adapter via SetOplockChecker. Nil if no SMB adapter running.
var oplockChecker OplockChecker
var oplockCheckerMu sync.RWMutex

// SetOplockChecker sets the cross-protocol lease checker.
// Called by the SMB adapter when it initializes the OplockManager.
func SetOplockChecker(checker OplockChecker) {
	oplockCheckerMu.Lock()
	defer oplockCheckerMu.Unlock()
	oplockChecker = checker
}

// GetOplockChecker returns the current oplock checker, or nil if not set.
func GetOplockChecker() OplockChecker {
	oplockCheckerMu.RLock()
	defer oplockCheckerMu.RUnlock()
	return oplockChecker
}

// CheckAndBreakLeasesForWrite checks for SMB leases that conflict with a write
// operation and initiates breaks as needed.
//
// This is called by NFS WRITE handler before committing the write.
// If an SMB client holds a Write lease on the file, the lease must be broken
// and the client must flush its cached writes before the NFS write proceeds.
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: File handle being written to
//
// Returns:
//   - nil if no SMB adapter or no conflicting leases
//   - ErrLeaseBreakPending if a lease break was initiated (caller should wait)
//   - Other errors for system failures
func (s *MetadataService) CheckAndBreakLeasesForWrite(ctx context.Context, handle FileHandle) error {
	checker := GetOplockChecker()
	if checker == nil {
		return nil // No SMB adapter, no leases to break
	}
	return checker.CheckAndBreakForWrite(ctx, lock.FileHandle(handle))
}

// CheckAndBreakLeasesForRead checks for SMB leases that conflict with a read
// operation and initiates breaks as needed.
//
// This is called by NFS READ handler before reading.
// If an SMB client holds a Write lease on the file (uncommitted writes),
// the lease must be broken and the client must flush before NFS read proceeds.
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: File handle being read from
//
// Returns:
//   - nil if no SMB adapter or no conflicting leases
//   - ErrLeaseBreakPending if a lease break was initiated (caller should wait)
//   - Other errors for system failures
func (s *MetadataService) CheckAndBreakLeasesForRead(ctx context.Context, handle FileHandle) error {
	checker := GetOplockChecker()
	if checker == nil {
		return nil // No SMB adapter, no leases to break
	}
	return checker.CheckAndBreakForRead(ctx, lock.FileHandle(handle))
}

// CheckAndBreakLeasesForDelete checks for SMB Handle leases that must break
// before file deletion.
//
// This is called by NFS REMOVE/RENAME handlers before deleting a file.
// SMB clients use Handle leases (H) to protect against "surprise deletion" -
// they expect to receive notification before a file they have open is deleted.
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: File handle being deleted
//
// Returns:
//   - nil if no SMB adapter or no Handle leases
//   - ErrLeaseBreakPending if a lease break was initiated (caller should wait)
//   - Other errors for system failures
func (s *MetadataService) CheckAndBreakLeasesForDelete(ctx context.Context, handle FileHandle) error {
	checker := GetOplockChecker()
	if checker == nil {
		return nil // No SMB adapter, no leases to break
	}
	return checker.CheckAndBreakForDelete(ctx, lock.FileHandle(handle))
}

// ============================================================================
// SMB Lease Reclaim Operations
// ============================================================================

// ReclaimLeaseSMB attempts to reclaim an SMB lease during grace period.
//
// This is called when an SMB client reconnects after server restart and
// wants to reclaim its previously held leases. The method verifies the lease
// existed in persistent storage before restart.
//
// Per CONTEXT.md decisions:
//   - Single shared 90-second grace period for both NFS and SMB
//   - Verify reclaims against persisted lock state
//   - Allow reclaim only if lease existed before restart
//   - If not in grace period, allow the reclaim anyway (backward compat - acts like new lease)
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: File handle for the lease
//   - leaseKey: The 16-byte SMB lease key
//   - clientID: Client identifier (e.g., "smb:{session_id}")
//   - requestedState: The lease state being reclaimed (R/W/H flags)
//
// Returns:
//   - *lock.LockResult: Contains the reclaimed lease on success
//   - error: ErrLockNotFound if no lease to reclaim, other errors for system failures
func (s *MetadataService) ReclaimLeaseSMB(
	ctx context.Context,
	handle FileHandle,
	leaseKey [16]byte,
	clientID string,
	requestedState uint32,
) (*lock.LockResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Get the store for this handle's share
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}

	// Check if the store implements LockStore
	lockStore, ok := store.(lock.LockStore)
	if !ok {
		// Store doesn't support lock persistence - return not found
		return nil, &errors.StoreError{
			Code:    errors.ErrLockNotFound,
			Message: "lease not found for reclaim (store does not support lock persistence)",
			Path:    string(handle),
		}
	}

	// Try to reclaim the lease from the store
	// This validates the lease existed in persistent storage before restart
	reclaimedLock, err := lockStore.ReclaimLease(ctx, lock.FileHandle(handle), leaseKey, clientID)
	if err != nil {
		return nil, err
	}

	// Update the lease state if different from what was requested
	// (client may be reclaiming with a different state)
	if reclaimedLock.Lease != nil && reclaimedLock.Lease.LeaseState != requestedState {
		// TODO: Log the reclaim with state difference at INFO level per CONTEXT.md
		// The actual state reconciliation is handled by the caller
		_ = requestedState // acknowledge state difference for future reconciliation
	}

	// Mark as reclaimed
	reclaimedLock.Reclaim = true
	if reclaimedLock.Lease != nil {
		reclaimedLock.Lease.Reclaim = true
	}

	// Get the share name for grace period tracking
	shareName, _, err := DecodeFileHandle(handle)
	if err == nil {
		// Try to notify grace period manager for early exit tracking
		// This is best-effort - grace period may not be active
		s.mu.RLock()
		view := s.unifiedViews[shareName]
		s.mu.RUnlock()
		if view != nil {
			// TODO: Notify grace period manager for early exit tracking
			// Currently grace period managers are managed externally (by adapters)
			// so we log the reclaim event for monitoring
			_ = view // grace period notification to be implemented
		}
	}

	return &lock.LockResult{
		Success: true,
		Lock:    reclaimedLock,
	}, nil
}
