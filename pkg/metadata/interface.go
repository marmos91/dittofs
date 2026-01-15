package metadata

import "context"

// MetadataServiceInterface defines all public operations provided by MetadataService.
//
// This interface documents the complete API available to protocol handlers
// and other consumers of the metadata layer. All methods handle business logic
// including permission checking, validation, and store routing.
//
// The interface is organized into logical groups:
//   - Store Management: Register and retrieve metadata stores per share
//   - File Operations: CRUD operations on files and symlinks
//   - Directory Operations: Directory creation, removal, and listing
//   - I/O Operations: Prepare/commit patterns for read/write coordination
//   - Permission Operations: Access control checks
//   - Locking Operations: Byte-range locking for SMB/NLM
//   - Share Operations: Share configuration and access control
//   - Handle Operations: File handle and filesystem metadata
type MetadataServiceInterface interface {
	// ========================================================================
	// Store Management
	// ========================================================================

	// RegisterStoreForShare associates a metadata store with a share.
	// Creates a LockManager for the share if one doesn't exist.
	RegisterStoreForShare(shareName string, store MetadataStore) error

	// GetStoreForShare returns the metadata store for a specific share.
	GetStoreForShare(shareName string) (MetadataStore, error)

	// ========================================================================
	// File Operations
	// ========================================================================

	// GetFile retrieves file metadata by handle (no permission checking).
	GetFile(ctx context.Context, handle FileHandle) (*File, error)

	// Lookup resolves a name in a directory to a File.
	// Checks read and execute permissions on the parent directory.
	Lookup(ctx *AuthContext, dirHandle FileHandle, name string) (*File, error)

	// CreateFile creates a new regular file in a directory.
	CreateFile(ctx *AuthContext, parentHandle FileHandle, name string, attr *FileAttr) (*File, error)

	// RemoveFile removes a file from its parent directory.
	// Handles hard link count decrements and orphan marking.
	RemoveFile(ctx *AuthContext, parentHandle FileHandle, name string) (*File, error)

	// CreateSymlink creates a symbolic link.
	CreateSymlink(ctx *AuthContext, parentHandle FileHandle, name string, target string, attr *FileAttr) (*File, error)

	// ReadSymlink reads the target of a symbolic link.
	ReadSymlink(ctx *AuthContext, handle FileHandle) (string, *File, error)

	// CreateSpecialFile creates a special file (FIFO, socket, block/char device).
	CreateSpecialFile(ctx *AuthContext, parentHandle FileHandle, name string, fileType FileType, attr *FileAttr, deviceMajor, deviceMinor uint32) (*File, error)

	// CreateHardLink creates a hard link to an existing file.
	CreateHardLink(ctx *AuthContext, dirHandle FileHandle, name string, targetHandle FileHandle) error

	// SetFileAttributes modifies file attributes (mode, owner, size, times).
	SetFileAttributes(ctx *AuthContext, handle FileHandle, attrs *SetAttrs) error

	// Move renames/moves a file or directory.
	Move(ctx *AuthContext, fromDir FileHandle, fromName string, toDir FileHandle, toName string) error

	// MarkFileAsOrphaned marks a file as orphaned (for background cleanup).
	MarkFileAsOrphaned(ctx *AuthContext, handle FileHandle) error

	// ========================================================================
	// Directory Operations
	// ========================================================================

	// ReadDirectory reads directory entries with pagination.
	// Cookie is an opaque uint64 value (0 = start from beginning).
	ReadDirectory(ctx *AuthContext, dirHandle FileHandle, cookie uint64, maxBytes uint32) (*ReadDirPage, error)

	// CreateDirectory creates a new directory.
	CreateDirectory(ctx *AuthContext, parentHandle FileHandle, name string, attr *FileAttr) (*File, error)

	// RemoveDirectory removes an empty directory.
	RemoveDirectory(ctx *AuthContext, parentHandle FileHandle, name string) error

	// ========================================================================
	// I/O Operations
	// ========================================================================

	// PrepareWrite validates a write operation and returns a WriteOperation intent.
	// Does NOT modify metadata - call CommitWrite after successful content write.
	PrepareWrite(ctx *AuthContext, handle FileHandle, newSize uint64) (*WriteOperation, error)

	// CommitWrite applies metadata changes after successful content write.
	CommitWrite(ctx *AuthContext, intent *WriteOperation) (*File, error)

	// PrepareRead validates a read operation and returns file metadata.
	PrepareRead(ctx *AuthContext, handle FileHandle) (*ReadMetadata, error)

	// ========================================================================
	// Permission Operations
	// ========================================================================

	// CheckPermissions performs file-level permission checking.
	// Returns the subset of requested permissions that are granted.
	CheckPermissions(ctx *AuthContext, handle FileHandle, requested Permission) (Permission, error)

	// ========================================================================
	// Locking Operations (SMB/NLM byte-range locks)
	// ========================================================================

	// CheckLockForIO checks if an I/O operation conflicts with existing locks.
	// Lightweight check that doesn't verify file existence.
	CheckLockForIO(ctx context.Context, handle FileHandle, sessionID, offset, length uint64, isWrite bool) error

	// LockFile acquires a byte-range lock on a file.
	// Validates file exists, is not a directory, and user has appropriate permission.
	LockFile(ctx *AuthContext, handle FileHandle, lock FileLock) error

	// UnlockFile releases a byte-range lock.
	UnlockFile(ctx context.Context, handle FileHandle, sessionID, offset, length uint64) error

	// UnlockAllForSession releases all locks held by a session on a file.
	UnlockAllForSession(ctx context.Context, handle FileHandle, sessionID uint64) error

	// TestLock tests if a lock would conflict with existing locks.
	TestLock(ctx *AuthContext, handle FileHandle, sessionID, offset, length uint64, exclusive bool) (bool, *LockConflict, error)

	// ListLocks lists all locks on a file.
	ListLocks(ctx *AuthContext, handle FileHandle) ([]FileLock, error)

	// RemoveFileLocks removes all locks for a file (called when file is deleted).
	RemoveFileLocks(handle FileHandle)

	// ========================================================================
	// Share Operations
	// ========================================================================

	// CreateShare creates a new share with its root directory.
	CreateShare(ctx context.Context, shareName string, share *Share) error

	// GetShareOptions returns the configuration options for a share.
	GetShareOptions(ctx context.Context, shareName string) (*ShareOptions, error)

	// CheckShareAccess validates client access to a share and returns auth context.
	CheckShareAccess(ctx context.Context, shareName, clientAddr, authMethod string, identity *Identity) (*AccessDecision, *AuthContext, error)

	// ========================================================================
	// Handle and Filesystem Operations
	// ========================================================================

	// GetChild resolves a name in a directory to a file handle.
	GetChild(ctx context.Context, dirHandle FileHandle, name string) (FileHandle, error)

	// GetRootHandle returns the root handle for a share.
	GetRootHandle(ctx context.Context, shareName string) (FileHandle, error)

	// GenerateHandle generates a new file handle for a path.
	GenerateHandle(ctx context.Context, shareName, path string) (FileHandle, error)

	// GetFilesystemStatistics returns dynamic filesystem usage statistics.
	GetFilesystemStatistics(ctx context.Context, handle FileHandle) (*FilesystemStatistics, error)

	// GetFilesystemCapabilities returns static filesystem capabilities.
	GetFilesystemCapabilities(ctx context.Context, handle FileHandle) (*FilesystemCapabilities, error)
}

// Compile-time check that MetadataService implements MetadataServiceInterface.
var _ MetadataServiceInterface = (*MetadataService)(nil)
