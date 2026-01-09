package metadata

import (
	"context"
)

// ============================================================================
// Transaction Interface
// ============================================================================

// Transaction provides transactional CRUD operations for metadata storage.
//
// All operations within a transaction see a consistent snapshot and are
// committed atomically when the transaction function returns without error.
//
// Transaction implementations vary by store:
//   - Memory store: Uses mutex locking (lock held for entire transaction)
//   - BadgerDB: Uses native Badger transactions
//   - PostgreSQL: Uses SQL transactions with BEGIN/COMMIT/ROLLBACK
//
// Thread Safety:
// Transaction objects are NOT safe for concurrent use. Each goroutine should
// use its own transaction via WithTransaction.
type Transaction interface {
	// ========================================================================
	// File Entry Operations
	// ========================================================================

	// GetEntry retrieves file metadata by handle.
	// Returns ErrNotFound if handle doesn't exist.
	// NO permission checking - caller is responsible.
	GetEntry(ctx context.Context, handle FileHandle) (*File, error)

	// PutEntry stores or updates file metadata.
	// Creates the entry if it doesn't exist, updates if it does.
	// NO validation - caller is responsible for data integrity.
	PutEntry(ctx context.Context, file *File) error

	// DeleteEntry removes file metadata by handle.
	// Returns ErrNotFound if handle doesn't exist.
	// Does NOT check if the file has children or is still referenced.
	DeleteEntry(ctx context.Context, handle FileHandle) error

	// ========================================================================
	// Directory Child Operations
	// ========================================================================

	// GetChild resolves a name in a directory to a file handle.
	// Returns the handle of the child, or ErrNotFound if name doesn't exist.
	// NO directory type checking - caller must verify parent is a directory.
	GetChild(ctx context.Context, dirHandle FileHandle, name string) (FileHandle, error)

	// SetChild adds or updates a child entry in a directory.
	// Creates the mapping: dirHandle -> (name -> childHandle)
	// Overwrites existing mapping if name already exists.
	SetChild(ctx context.Context, dirHandle FileHandle, name string, childHandle FileHandle) error

	// DeleteChild removes a child entry from a directory.
	// Returns ErrNotFound if name doesn't exist in the directory.
	DeleteChild(ctx context.Context, dirHandle FileHandle, name string) error

	// ListChildren returns directory entries with pagination support.
	// cursor: Pagination token (empty string = start from beginning)
	// limit: Maximum entries to return (0 = use default)
	// Returns: entries, nextCursor (empty if no more), error
	ListChildren(ctx context.Context, dirHandle FileHandle, cursor string, limit int) ([]DirEntry, string, error)

	// ========================================================================
	// Parent Tracking Operations
	// ========================================================================

	// GetParent returns the parent handle for a file/directory.
	// Returns ErrNotFound for root directories (no parent).
	GetParent(ctx context.Context, handle FileHandle) (FileHandle, error)

	// SetParent sets the parent handle for a file/directory.
	// Used when creating files or moving files between directories.
	SetParent(ctx context.Context, handle FileHandle, parentHandle FileHandle) error

	// ========================================================================
	// Link Count Operations
	// ========================================================================

	// GetLinkCount returns the hard link count for a file.
	// Returns 0 if the file doesn't track link counts or doesn't exist.
	GetLinkCount(ctx context.Context, handle FileHandle) (uint32, error)

	// SetLinkCount sets the hard link count for a file.
	// Used for hard link management and orphan detection (nlink=0).
	SetLinkCount(ctx context.Context, handle FileHandle, count uint32) error

	// ========================================================================
	// Filesystem Metadata Operations
	// ========================================================================

	// GetFilesystemMeta retrieves filesystem metadata for a share.
	// This includes capabilities and statistics stored as a single entry.
	// Returns ErrNotFound if metadata doesn't exist for the share.
	GetFilesystemMeta(ctx context.Context, shareName string) (*FilesystemMeta, error)

	// PutFilesystemMeta stores filesystem metadata for a share.
	// Creates or updates the metadata entry.
	PutFilesystemMeta(ctx context.Context, shareName string, meta *FilesystemMeta) error
}

// ============================================================================
// Transactor Interface
// ============================================================================

// Transactor provides transaction support for metadata operations.
//
// Stores that support transactions implement this interface to ensure
// atomic operations across multiple CRUD calls.
//
// Usage pattern:
//
//	err := store.WithTransaction(ctx, func(tx Transaction) error {
//	    // All operations within this function are atomic
//	    entry, err := tx.GetEntry(ctx, handle)
//	    if err != nil {
//	        return err  // Transaction will be rolled back
//	    }
//
//	    // Modify entry...
//
//	    return tx.PutEntry(ctx, entry)  // Success = commit, error = rollback
//	})
type Transactor interface {
	// WithTransaction executes fn within a transaction.
	//
	// If fn returns an error, the transaction is rolled back.
	// If fn returns nil, the transaction is committed.
	//
	// The Transaction object passed to fn should only be used within fn.
	// Using it after fn returns has undefined behavior.
	//
	// Nested transactions are NOT supported. Calling WithTransaction from
	// within fn will either fail or start an independent transaction
	// (implementation-dependent).
	WithTransaction(ctx context.Context, fn func(tx Transaction) error) error
}

// ============================================================================
// Locker Interface
// ============================================================================

// Locker provides file locking for SMB/NLM protocols.
//
// Locks are session-scoped, not transaction-scoped. They persist across
// multiple transactions and are released explicitly or when the session ends.
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
type Locker interface {
	// LockFile acquires a byte-range lock on a file.
	//
	// Permission Requirements:
	//   - Exclusive locks require write permission on the file
	//   - Shared locks require read permission on the file
	//
	// Parameters:
	//   - ctx: Authentication context for permission checking
	//   - handle: File handle to lock
	//   - lock: Lock details (ID, SessionID, Offset, Length, Exclusive, ClientAddr)
	//
	// Returns:
	//   - error: ErrLocked if conflict exists, ErrNotFound if file doesn't exist,
	//     ErrPermissionDenied if no permission
	LockFile(ctx *AuthContext, handle FileHandle, lock FileLock) error

	// UnlockFile releases a specific byte-range lock.
	//
	// The lock is identified by session, offset, and length - all must match exactly.
	//
	// Returns:
	//   - error: ErrLockNotFound if lock doesn't exist, ErrNotFound if file doesn't exist
	UnlockFile(ctx context.Context, handle FileHandle, sessionID, offset, length uint64) error

	// UnlockAllForSession releases all locks held by a session on a file.
	//
	// Called when a client closes a file or disconnects.
	UnlockAllForSession(ctx context.Context, handle FileHandle, sessionID uint64) error

	// TestLock checks whether a lock would succeed without acquiring it.
	//
	// Returns:
	//   - bool: true if lock would succeed, false if conflict exists
	//   - *LockConflict: Details of conflicting lock if bool is false
	//   - error: ErrNotFound if file doesn't exist
	TestLock(ctx context.Context, handle FileHandle, sessionID, offset, length uint64, exclusive bool) (bool, *LockConflict, error)

	// CheckLockForIO verifies no conflicting locks exist for a read/write operation.
	//
	// Returns:
	//   - error: nil if I/O is allowed, ErrLocked if blocked
	CheckLockForIO(ctx context.Context, handle FileHandle, sessionID, offset, length uint64, isWrite bool) error

	// ListLocks returns all active locks on a file.
	//
	// Returns:
	//   - []FileLock: All active locks on the file (empty slice if none)
	ListLocks(ctx context.Context, handle FileHandle) ([]FileLock, error)
}

// ============================================================================
// FilesystemMeta
// ============================================================================

// FilesystemMeta holds persisted filesystem information.
//
// This combines capabilities and statistics into a single persistable structure
// that can be stored and retrieved via the Transaction interface.
type FilesystemMeta struct {
	// Capabilities contains static filesystem capabilities and limits
	Capabilities FilesystemCapabilities

	// Statistics contains dynamic filesystem usage statistics
	Statistics FilesystemStatistics
}

// ============================================================================
// MetadataStore Interface
// ============================================================================

// MetadataStore is the main interface for metadata operations.
//
// It combines:
//   - Transaction: CRUD operations (for non-transactional use)
//   - Transactor: Transaction support for atomic operations
//   - Locker: File locking for SMB/NLM
//
// Plus additional operations for share/handle management and health.
//
// Design Principles:
//   - Protocol-agnostic: No NFS/SMB/FTP-specific types or values
//   - Consistent error handling: All operations return StoreError for business logic errors
//   - Context-aware: All operations respect context cancellation and timeouts
//   - Atomic operations: Use WithTransaction for multi-step operations
//
// Thread Safety:
// Implementations must be safe for concurrent use by multiple goroutines.
type MetadataStore interface {
	Transaction // CRUD operations (non-transactional calls)
	Transactor  // Transaction support for atomic operations
	Locker      // File locking for SMB/NLM

	// ========================================================================
	// Handle/Share Management
	// ========================================================================

	// GenerateHandle creates a new unique file handle for a path in a share.
	// The handle format is implementation-specific but must be stable.
	// Format: "shareName:path" or "shareName:uuid" depending on implementation.
	GenerateHandle(ctx context.Context, shareName string, path string) (FileHandle, error)

	// GetRootHandle returns the root handle for a share.
	// Returns ErrNotFound if the share doesn't exist.
	GetRootHandle(ctx context.Context, shareName string) (FileHandle, error)

	// GetShareNameForHandle returns the share name for a given file handle.
	// Returns ErrInvalidHandle if handle is malformed.
	GetShareNameForHandle(ctx context.Context, handle FileHandle) (string, error)

	// GetShareOptions returns the share configuration options.
	// Used by business logic to check permissions, identity mapping, etc.
	// Returns ErrNotFound if the share doesn't exist.
	GetShareOptions(ctx context.Context, shareName string) (*ShareOptions, error)

	// ========================================================================
	// Share Lifecycle
	// ========================================================================

	// CreateShare creates a new share with the given configuration.
	// Also creates the root directory for the share.
	// Returns ErrAlreadyExists if share already exists.
	CreateShare(ctx context.Context, share *Share) error

	// DeleteShare removes a share and all its metadata.
	// Returns ErrNotFound if share doesn't exist.
	// WARNING: This does NOT delete content from the content store.
	DeleteShare(ctx context.Context, shareName string) error

	// ListShares returns the names of all shares.
	ListShares(ctx context.Context) ([]string, error)

	// ========================================================================
	// High-Level Operations (use transactions internally)
	// ========================================================================
	//
	// These methods implement business logic with proper permission checking
	// and atomicity. They use WithTransaction internally to ensure consistency.

	// Lookup resolves a name within a directory to a file handle and attributes.
	// Handles special names "." and "..".
	// Returns ErrNotFound if name doesn't exist, ErrAccessDenied if no permission.
	Lookup(ctx *AuthContext, dirHandle FileHandle, name string) (*File, error)

	// GetFile retrieves complete file information by handle.
	// This is a lightweight operation without permission checking.
	GetFile(ctx context.Context, handle FileHandle) (*File, error)

	// SetFileAttributes updates file attributes with validation and access control.
	// Only attributes with non-nil pointers in attrs are modified.
	SetFileAttributes(ctx *AuthContext, handle FileHandle, attrs *SetAttrs) error

	// CheckPermissions performs file-level permission checking.
	// Returns granted permissions (subset of requested).
	CheckPermissions(ctx *AuthContext, handle FileHandle, requested Permission) (Permission, error)

	// ========================================================================
	// File/Directory Creation
	// ========================================================================

	// CreateRootDirectory creates a root directory for a share without a parent.
	// Called during share initialization.
	CreateRootDirectory(ctx context.Context, shareName string, attr *FileAttr) (*File, error)

	// Create creates a new file or directory.
	// attr.Type must be FileTypeRegular or FileTypeDirectory.
	Create(ctx *AuthContext, parentHandle FileHandle, name string, attr *FileAttr) (*File, error)

	// CreateSymlink creates a symbolic link pointing to a target path.
	CreateSymlink(ctx *AuthContext, parentHandle FileHandle, name string, target string, attr *FileAttr) (*File, error)

	// CreateSpecialFile creates a special file (device, socket, or FIFO).
	CreateSpecialFile(ctx *AuthContext, parentHandle FileHandle, name string, fileType FileType, attr *FileAttr, deviceMajor, deviceMinor uint32) (*File, error)

	// CreateHardLink creates a hard link to an existing file.
	CreateHardLink(ctx *AuthContext, dirHandle FileHandle, name string, targetHandle FileHandle) error

	// ========================================================================
	// File/Directory Removal
	// ========================================================================

	// RemoveFile removes a file's metadata from its parent directory.
	// Returns the removed file's attributes (includes ContentID for content cleanup).
	// WARNING: Does NOT delete content - caller must handle content cleanup.
	RemoveFile(ctx *AuthContext, parentHandle FileHandle, name string) (*File, error)

	// RemoveDirectory removes an empty directory's metadata from its parent.
	RemoveDirectory(ctx *AuthContext, parentHandle FileHandle, name string) error

	// ========================================================================
	// File/Directory Operations
	// ========================================================================

	// Move moves or renames a file or directory atomically.
	// Can replace existing files/directories at destination.
	Move(ctx *AuthContext, fromDir FileHandle, fromName string, toDir FileHandle, toName string) error

	// ReadSymlink reads the target path of a symbolic link.
	ReadSymlink(ctx *AuthContext, handle FileHandle) (string, *File, error)

	// ReadDirectory reads one page of directory entries with pagination support.
	ReadDirectory(ctx *AuthContext, dirHandle FileHandle, token string, maxBytes uint32) (*ReadDirPage, error)

	// ========================================================================
	// File Content Coordination
	// ========================================================================

	// PrepareWrite validates a write operation and returns a write intent.
	// Does NOT modify metadata - use CommitWrite after successful content write.
	PrepareWrite(ctx *AuthContext, handle FileHandle, newSize uint64) (*WriteOperation, error)

	// CommitWrite applies metadata changes after a successful content write.
	CommitWrite(ctx *AuthContext, intent *WriteOperation) (*File, error)

	// PrepareRead validates a read operation and returns file metadata.
	PrepareRead(ctx *AuthContext, handle FileHandle) (*ReadMetadata, error)

	// ========================================================================
	// Filesystem Information
	// ========================================================================

	// GetFilesystemCapabilities returns static filesystem capabilities and limits.
	GetFilesystemCapabilities(ctx context.Context, handle FileHandle) (*FilesystemCapabilities, error)

	// SetFilesystemCapabilities updates the filesystem capabilities for this store.
	SetFilesystemCapabilities(capabilities FilesystemCapabilities)

	// GetFilesystemStatistics returns dynamic filesystem usage statistics.
	GetFilesystemStatistics(ctx context.Context, handle FileHandle) (*FilesystemStatistics, error)

	// ========================================================================
	// Configuration & Health
	// ========================================================================

	// SetServerConfig sets the server-wide configuration.
	SetServerConfig(ctx context.Context, config MetadataServerConfig) error

	// GetServerConfig returns the current server configuration.
	GetServerConfig(ctx context.Context) (MetadataServerConfig, error)

	// Healthcheck verifies the repository is operational.
	Healthcheck(ctx context.Context) error

	// Close releases any resources held by the store.
	Close() error

	// ========================================================================
	// Content ID Operations
	// ========================================================================

	// GetFileByContentID retrieves file metadata by its content identifier.
	// Used by the background flusher to validate cached data.
	GetFileByContentID(ctx context.Context, contentID ContentID) (*File, error)
}
