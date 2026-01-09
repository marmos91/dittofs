package metadata

import (
	"context"
	"fmt"
	"sync"
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
	mu           sync.RWMutex
	stores       map[string]MetadataStore // shareName -> store
	lockManagers map[string]*LockManager  // shareName -> lock manager (ephemeral, per-share)
}

// New creates a new empty MetadataService instance.
// Use RegisterStoreForShare to configure stores for each share.
func New() *MetadataService {
	return &MetadataService{
		stores:       make(map[string]MetadataStore),
		lockManagers: make(map[string]*LockManager),
	}
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

// ============================================================================
// Low-Level Store Operations
// ============================================================================
// These methods provide direct access to store operations without additional
// business logic. They route to the correct store based on the handle's share.

// GetFile retrieves file metadata by handle.
// This is a convenience method that calls GetFile from the Base interface.
func (s *MetadataService) GetFile(ctx context.Context, handle FileHandle) (*File, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}
	return store.GetFile(ctx, handle)
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
	store, err := s.storeForHandle(handle)
	if err != nil {
		return 0, err
	}
	return CheckFilePermissions(store, ctx, handle, requested)
}

// GetChild retrieves a child's handle from a directory.
func (s *MetadataService) GetChild(ctx context.Context, dirHandle FileHandle, name string) (FileHandle, error) {
	store, err := s.storeForHandle(dirHandle)
	if err != nil {
		return nil, err
	}
	return store.GetChild(ctx, dirHandle, name)
}

// ReadDirectory reads directory entries with pagination.
func (s *MetadataService) ReadDirectory(ctx *AuthContext, dirHandle FileHandle, cursor string, limit uint32) (*ReadDirPage, error) {
	store, err := s.storeForHandle(dirHandle)
	if err != nil {
		return nil, err
	}
	return ReadDirectory(store, ctx, dirHandle, cursor, limit)
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
// Business logic (permission checking, file type validation) is handled here.
// See LockFile function in locks.go for details.
func (s *MetadataService) LockFile(ctx *AuthContext, handle FileHandle, lock FileLock) error {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return err
	}

	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return err
	}

	// Use business logic function which handles permission checking
	return lockFileWithManager(store, lm, ctx, handle, lock)
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
func (s *MetadataService) TestLock(ctx *AuthContext, handle FileHandle, sessionID, offset, length uint64, exclusive bool) (bool, *LockConflict, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return false, nil, err
	}

	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return false, nil, err
	}

	// Use business logic function which verifies file exists
	return testFileLockWithManager(store, lm, ctx, handle, sessionID, offset, length, exclusive)
}

// ListLocks lists all locks on a file.
func (s *MetadataService) ListLocks(ctx *AuthContext, handle FileHandle) ([]FileLock, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}

	lm, err := s.lockManagerForHandle(handle)
	if err != nil {
		return nil, err
	}

	// Use business logic function which verifies file exists
	return listFileLocksWithManager(store, lm, ctx, handle)
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

// CheckShareAccess verifies if a client can access a share and returns effective credentials.
// See CheckShareAccess function for detailed documentation.
func (s *MetadataService) CheckShareAccess(ctx context.Context, shareName, clientAddr, authMethod string, identity *Identity) (*AccessDecision, *AuthContext, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, nil, err
	}
	return CheckShareAccess(store, ctx, shareName, clientAddr, authMethod, identity)
}

// ============================================================================
// High-Level File Operations (with business logic)
// ============================================================================

// RemoveFile removes a file from its parent directory.
// See RemoveFile function for detailed documentation.
func (s *MetadataService) RemoveFile(ctx *AuthContext, parentHandle FileHandle, name string) (*File, error) {
	store, err := s.storeForHandle(parentHandle)
	if err != nil {
		return nil, err
	}
	return RemoveFile(store, ctx, parentHandle, name)
}

// Lookup resolves a name in a directory to a file.
// See Lookup function for detailed documentation.
func (s *MetadataService) Lookup(ctx *AuthContext, parentHandle FileHandle, name string) (*File, error) {
	store, err := s.storeForHandle(parentHandle)
	if err != nil {
		return nil, err
	}
	return Lookup(store, ctx, parentHandle, name)
}

// CreateFile creates a new regular file.
// See CreateFile function for detailed documentation.
func (s *MetadataService) CreateFile(ctx *AuthContext, parentHandle FileHandle, name string, attr *FileAttr) (*File, error) {
	store, err := s.storeForHandle(parentHandle)
	if err != nil {
		return nil, err
	}
	return CreateFile(store, ctx, parentHandle, name, attr)
}

// CreateSymlink creates a symbolic link.
// See CreateSymlink function for detailed documentation.
func (s *MetadataService) CreateSymlink(ctx *AuthContext, parentHandle FileHandle, name string, target string, attr *FileAttr) (*File, error) {
	store, err := s.storeForHandle(parentHandle)
	if err != nil {
		return nil, err
	}
	return CreateSymlink(store, ctx, parentHandle, name, target, attr)
}

// CreateSpecialFile creates a special file (device, socket, FIFO).
// See CreateSpecialFile function for detailed documentation.
func (s *MetadataService) CreateSpecialFile(ctx *AuthContext, parentHandle FileHandle, name string, fileType FileType, attr *FileAttr, major, minor uint32) (*File, error) {
	store, err := s.storeForHandle(parentHandle)
	if err != nil {
		return nil, err
	}
	return CreateSpecialFile(store, ctx, parentHandle, name, fileType, attr, major, minor)
}

// CreateHardLink creates a hard link to an existing file.
// See CreateHardLink function for detailed documentation.
func (s *MetadataService) CreateHardLink(ctx *AuthContext, dirHandle FileHandle, name string, targetHandle FileHandle) error {
	store, err := s.storeForHandle(dirHandle)
	if err != nil {
		return err
	}
	return CreateHardLink(store, ctx, dirHandle, name, targetHandle)
}

// ReadSymlink reads the target of a symbolic link.
// See ReadSymlink function for detailed documentation.
func (s *MetadataService) ReadSymlink(ctx *AuthContext, handle FileHandle) (string, *File, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return "", nil, err
	}
	return ReadSymlink(store, ctx, handle)
}

// SetFileAttributes updates file attributes.
// See SetFileAttributes function for detailed documentation.
func (s *MetadataService) SetFileAttributes(ctx *AuthContext, handle FileHandle, attrs *SetAttrs) error {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return err
	}
	return SetFileAttributes(store, ctx, handle, attrs)
}

// Move renames or moves a file/directory.
// See Move function for detailed documentation.
func (s *MetadataService) Move(ctx *AuthContext, fromDir FileHandle, fromName string, toDir FileHandle, toName string) error {
	store, err := s.storeForHandle(fromDir)
	if err != nil {
		return err
	}
	return Move(store, ctx, fromDir, fromName, toDir, toName)
}

// MarkFileAsOrphaned marks a file as orphaned (unlinked but still open).
// See MarkFileAsOrphaned function for detailed documentation.
func (s *MetadataService) MarkFileAsOrphaned(ctx *AuthContext, handle FileHandle) error {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return err
	}
	return MarkFileAsOrphaned(store, ctx, handle)
}

// ============================================================================
// Directory Operations
// ============================================================================

// RemoveDirectory removes an empty directory.
// See RemoveDirectory function for detailed documentation.
func (s *MetadataService) RemoveDirectory(ctx *AuthContext, parentHandle FileHandle, name string) error {
	store, err := s.storeForHandle(parentHandle)
	if err != nil {
		return err
	}
	return RemoveDirectory(store, ctx, parentHandle, name)
}

// CreateDirectory creates a new directory.
// See CreateDirectory function for detailed documentation.
func (s *MetadataService) CreateDirectory(ctx *AuthContext, parentHandle FileHandle, name string, attr *FileAttr) (*File, error) {
	store, err := s.storeForHandle(parentHandle)
	if err != nil {
		return nil, err
	}
	return CreateDirectory(store, ctx, parentHandle, name, attr)
}

// ============================================================================
// I/O Operations
// ============================================================================

// PrepareWrite prepares a write operation.
// See PrepareWrite function for detailed documentation.
func (s *MetadataService) PrepareWrite(ctx *AuthContext, handle FileHandle, newSize uint64) (*WriteOperation, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}
	return PrepareWrite(store, ctx, handle, newSize)
}

// CommitWrite commits a write operation after content is written.
// See CommitWrite function for detailed documentation.
func (s *MetadataService) CommitWrite(ctx *AuthContext, intent *WriteOperation) (*File, error) {
	store, err := s.storeForHandle(intent.Handle)
	if err != nil {
		return nil, err
	}
	return CommitWrite(store, ctx, intent)
}

// PrepareRead prepares a read operation.
// See PrepareRead function for detailed documentation.
func (s *MetadataService) PrepareRead(ctx *AuthContext, handle FileHandle) (*ReadMetadata, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}
	return PrepareRead(store, ctx, handle)
}
