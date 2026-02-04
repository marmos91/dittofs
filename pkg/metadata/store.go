package metadata

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
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
// ObjectStore Interface (Content-Addressed Deduplication)
// ============================================================================

// ObjectStore defines operations for content-addressed object management.
//
// This interface enables deduplication by tracking objects, chunks, and blocks
// using content hashes. When deduplication is enabled:
//   - Objects are identified by their content hash (SHA-256)
//   - Identical files share the same Object (via RefCount)
//   - Chunks and blocks can be shared across objects
//
// Note: ObjectStore operations are optional. Stores may return ErrNotSupported
// if deduplication is not enabled or supported.
type ObjectStore interface {
	// ========================================================================
	// Object Operations
	// ========================================================================

	// GetObject retrieves an object by its content hash.
	// Returns ErrObjectNotFound if not found.
	GetObject(ctx context.Context, id ContentHash) (*Object, error)

	// PutObject stores or updates an object.
	// If an object with the same ID exists, it updates the metadata.
	PutObject(ctx context.Context, obj *Object) error

	// DeleteObject removes an object by its content hash.
	// Returns ErrObjectNotFound if not found.
	// WARNING: Only call when RefCount is 0. Does NOT cascade delete chunks/blocks.
	DeleteObject(ctx context.Context, id ContentHash) error

	// IncrementObjectRefCount atomically increments an object's RefCount.
	// Returns the new RefCount, or ErrObjectNotFound if not found.
	IncrementObjectRefCount(ctx context.Context, id ContentHash) (uint32, error)

	// DecrementObjectRefCount atomically decrements an object's RefCount.
	// Returns the new RefCount, or ErrObjectNotFound if not found.
	// Returns 0 when the object can be garbage collected.
	DecrementObjectRefCount(ctx context.Context, id ContentHash) (uint32, error)

	// ========================================================================
	// Chunk Operations
	// ========================================================================

	// GetChunk retrieves a chunk by its content hash.
	// Returns ErrChunkNotFound if not found.
	GetChunk(ctx context.Context, hash ContentHash) (*ObjectChunk, error)

	// GetChunksByObject retrieves all chunks for an object.
	// Returns chunks ordered by Index (0, 1, 2, ...).
	GetChunksByObject(ctx context.Context, objectID ContentHash) ([]*ObjectChunk, error)

	// PutChunk stores or updates a chunk.
	// If a chunk with the same Hash exists, it updates the metadata.
	PutChunk(ctx context.Context, chunk *ObjectChunk) error

	// DeleteChunk removes a chunk by its content hash.
	// Returns ErrChunkNotFound if not found.
	// WARNING: Only call when RefCount is 0. Does NOT cascade delete blocks.
	DeleteChunk(ctx context.Context, hash ContentHash) error

	// IncrementChunkRefCount atomically increments a chunk's RefCount.
	// Returns the new RefCount, or ErrChunkNotFound if not found.
	IncrementChunkRefCount(ctx context.Context, hash ContentHash) (uint32, error)

	// DecrementChunkRefCount atomically decrements a chunk's RefCount.
	// Returns the new RefCount, or ErrChunkNotFound if not found.
	// Returns 0 when the chunk can be garbage collected.
	DecrementChunkRefCount(ctx context.Context, hash ContentHash) (uint32, error)

	// ========================================================================
	// Block Operations
	// ========================================================================

	// GetBlock retrieves a block by its content hash.
	// Returns ErrBlockNotFound if not found.
	GetBlock(ctx context.Context, hash ContentHash) (*ObjectBlock, error)

	// GetBlocksByChunk retrieves all blocks for a chunk.
	// Returns blocks ordered by Index (0, 1, 2, ...).
	GetBlocksByChunk(ctx context.Context, chunkHash ContentHash) ([]*ObjectBlock, error)

	// PutBlock stores or updates a block.
	// If a block with the same Hash exists, it updates the metadata (including RefCount).
	PutBlock(ctx context.Context, block *ObjectBlock) error

	// DeleteBlock removes a block by its content hash.
	// Returns ErrBlockNotFound if not found.
	// WARNING: Only call when RefCount is 0.
	DeleteBlock(ctx context.Context, hash ContentHash) error

	// FindBlockByHash looks up a block by its content hash.
	// Returns the block if found, nil if not found (no error for not found).
	// This is used for deduplication: check if block already exists before upload.
	FindBlockByHash(ctx context.Context, hash ContentHash) (*ObjectBlock, error)

	// IncrementBlockRefCount atomically increments a block's RefCount.
	// Returns the new RefCount, or ErrBlockNotFound if not found.
	IncrementBlockRefCount(ctx context.Context, hash ContentHash) (uint32, error)

	// DecrementBlockRefCount atomically decrements a block's RefCount.
	// Returns the new RefCount, or ErrBlockNotFound if not found.
	// Returns 0 when the block can be garbage collected (safe to delete from block store).
	DecrementBlockRefCount(ctx context.Context, hash ContentHash) (uint32, error)

	// MarkBlockUploaded marks a block as uploaded to the block store.
	// Sets UploadedAt to current time. Returns ErrBlockNotFound if not found.
	MarkBlockUploaded(ctx context.Context, hash ContentHash) error
}

// ============================================================================
// Transaction Interface
// ============================================================================

// Transaction provides all operations available within a transactional context.
//
// This interface combines Files, Shares, ServerConfig, ObjectStore, and LockStore
// interfaces to enable atomic operations across all metadata domains.
type Transaction interface {
	Files           // File CRUD operations
	Shares          // Share management
	ServerConfig    // Server configuration
	ObjectStore     // Content-addressed deduplication (optional)
	lock.LockStore  // Lock persistence for NLM/SMB
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
// It combines five interfaces:
//   - Files: File CRUD operations (for non-transactional use and within transactions)
//   - Shares: Share lifecycle and handle management
//   - ServerConfig: Server configuration, capabilities, and health
//   - Transactor: Transaction support for atomic operations
//   - ObjectStore: Content-addressed deduplication (optional)
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
	ObjectStore  // Content-addressed deduplication (optional)

	// ========================================================================
	// Store Lifecycle (not transactional)
	// ========================================================================

	// Healthcheck verifies the store is operational.
	Healthcheck(ctx context.Context) error

	// Close releases any resources held by the store.
	Close() error
}
