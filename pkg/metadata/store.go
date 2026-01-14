package metadata

import (
	"context"
)

// ============================================================================
// Files Interface (File CRUD Operations)
// ============================================================================

// Files defines the core CRUD operations for file metadata storage.
//
// This interface is embedded by MetadataStore for direct (non-transactional) calls,
// and is also part of the Transaction interface for atomic operations.
//
// Implementations vary by store:
//   - Memory store: Uses mutex locking
//   - BadgerDB: Uses native Badger transactions
//   - PostgreSQL: Uses SQL transactions
//
// Thread Safety:
// Files objects from WithTransaction are NOT safe for concurrent use.
type Files interface {
	// ========================================================================
	// File Entry Operations
	// ========================================================================

	// GetFile retrieves file metadata by handle.
	// Returns ErrNotFound if handle doesn't exist.
	// NO permission checking - caller is responsible.
	GetFile(ctx context.Context, handle FileHandle) (*File, error)

	// PutFile stores or updates file metadata.
	// Creates the file if it doesn't exist, updates if it does.
	// NO validation - caller is responsible for data integrity.
	PutFile(ctx context.Context, file *File) error

	// DeleteFile removes file metadata by handle.
	// Returns ErrNotFound if handle doesn't exist.
	// Does NOT check if the file has children or is still referenced.
	DeleteFile(ctx context.Context, handle FileHandle) error

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
	// Handle Operations
	// ========================================================================

	// GenerateHandle creates a new unique file handle for a path in a share.
	// The handle format is implementation-specific but must be stable.
	// Format: "shareName:path" or "shareName:uuid" depending on implementation.
	GenerateHandle(ctx context.Context, shareName string, path string) (FileHandle, error)

	// ========================================================================
	// Content ID Operations
	// ========================================================================

	// GetFileByPayloadID retrieves file metadata by its content identifier.
	// Used by the background flusher to validate cached data.
	GetFileByPayloadID(ctx context.Context, payloadID PayloadID) (*File, error)

	// ========================================================================
	// Filesystem Metadata Operations
	// ========================================================================

	// GetFilesystemMeta retrieves filesystem metadata for a share.
	// This includes capabilities and statistics stored as a single entry.
	// Returns ErrNotFound if metadata doesn't exist for the share.
	GetFilesystemMeta(ctx context.Context, shareName string) (*FilesystemMeta, error)

	// PutFilesystemMeta stores filesystem metadata for a share.
	// Creates or updates the metadata entry.
	PutFilesystemMeta(ctx context.Context, shareName string, metaSvc *FilesystemMeta) error
}

// ============================================================================
// Shares Interface (Share Management)
// ============================================================================

// Shares defines operations for share lifecycle and handle management.
//
// These operations are typically non-transactional as they manage the
// share-level configuration rather than individual file operations.
type Shares interface {
	// ========================================================================
	// Share Access
	// ========================================================================

	// GetRootHandle returns the root handle for a share.
	// Returns ErrNotFound if the share doesn't exist.
	GetRootHandle(ctx context.Context, shareName string) (FileHandle, error)

	// GetShareOptions returns the share configuration options.
	// Used by business logic to check permissions, identity mapping, etc.
	// Returns ErrNotFound if the share doesn't exist.
	GetShareOptions(ctx context.Context, shareName string) (*ShareOptions, error)

	// ========================================================================
	// Share Lifecycle (CRUD)
	// ========================================================================

	// CreateShare creates a new share with the given configuration.
	// Also creates the root directory for the share.
	// Returns ErrAlreadyExists if share already exists.
	CreateShare(ctx context.Context, share *Share) error

	// UpdateShareOptions updates the share configuration options.
	// Returns ErrNotFound if share doesn't exist.
	UpdateShareOptions(ctx context.Context, shareName string, options *ShareOptions) error

	// DeleteShare removes a share and all its metadata.
	// Returns ErrNotFound if share doesn't exist.
	// WARNING: This does NOT delete content from the content store.
	DeleteShare(ctx context.Context, shareName string) error

	// ListShares returns the names of all shares.
	ListShares(ctx context.Context) ([]string, error)

	// ========================================================================
	// Root Directory Operations
	// ========================================================================

	// CreateRootDirectory creates a root directory for a share without a parent.
	// Called during share initialization.
	CreateRootDirectory(ctx context.Context, shareName string, attr *FileAttr) (*File, error)
}

// ============================================================================
// ServerConfig Interface (Server Configuration & Capabilities)
// ============================================================================

// ServerConfig defines operations for server configuration and capabilities.
//
// These operations manage server-level settings that apply across all shares
// and are safe to use within transactions.
type ServerConfig interface {
	// ========================================================================
	// Configuration
	// ========================================================================

	// SetServerConfig sets the server-wide configuration.
	SetServerConfig(ctx context.Context, config MetadataServerConfig) error

	// GetServerConfig returns the current server configuration.
	GetServerConfig(ctx context.Context) (MetadataServerConfig, error)

	// ========================================================================
	// Filesystem Capabilities & Statistics
	// ========================================================================

	// GetFilesystemCapabilities returns static filesystem capabilities and limits.
	GetFilesystemCapabilities(ctx context.Context, handle FileHandle) (*FilesystemCapabilities, error)

	// SetFilesystemCapabilities updates the filesystem capabilities for this store.
	SetFilesystemCapabilities(capabilities FilesystemCapabilities)

	// GetFilesystemStatistics returns dynamic filesystem usage statistics.
	GetFilesystemStatistics(ctx context.Context, handle FileHandle) (*FilesystemStatistics, error)
}

// ============================================================================
// Transaction Interface
// ============================================================================

// Transaction provides all operations available within a transactional context.
//
// This interface combines Files, Shares, and ServerConfig interfaces
// to enable atomic operations across all metadata domains.
type Transaction interface {
	Files        // File CRUD operations
	Shares       // Share management
	ServerConfig // Server configuration
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
//	    file, err := tx.GetFile(ctx, handle)
//	    if err != nil {
//	        return err  // Transaction will be rolled back
//	    }
//
//	    // Modify file...
//
//	    return tx.PutFile(ctx, file)  // Success = commit, error = rollback
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
// FilesystemMeta
// ============================================================================

// FilesystemMeta holds persisted filesystem information.
//
// This combines capabilities and statistics into a single persistable structure
// that can be stored and retrieved via the Base interface.
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
// It combines four interfaces:
//   - Base: File CRUD operations (for non-transactional use and within transactions)
//   - Shares: Share lifecycle and handle management
//   - Server: Server configuration, capabilities, and health
//   - Transactor: Transaction support for atomic operations
//
// Note: File locking (SMB/NLM) is handled separately by LockManager at the
// service level, not by individual stores. Locks are ephemeral (in-memory)
// and per-share, managed by MetadataService.
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
	Files        // File CRUD operations (non-transactional calls)
	Shares       // Share lifecycle and handle management
	ServerConfig // Server configuration and capabilities
	Transactor   // Transaction support for atomic operations

	// ========================================================================
	// Store Lifecycle (not transactional)
	// ========================================================================

	// Healthcheck verifies the store is operational.
	Healthcheck(ctx context.Context) error

	// Close releases any resources held by the store.
	Close() error
}
