package badger

import (
	"context"
	"fmt"
	"sync"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// BadgerMetadataStore implements metadata.MetadataStore using BadgerDB for persistence.
//
// This implementation provides a persistent metadata repository backed by BadgerDB,
// a fast embedded key-value store. It is suitable for:
//   - Production environments requiring persistence across restarts
//   - Systems where metadata must survive server crashes
//   - Deployments needing stable file handles across restarts
//   - Multi-GB metadata storage requirements
//
// Key Features:
//   - Persistent storage with crash recovery (WAL-based)
//   - Path-based file handles for import/export capability
//   - ACID transactions for complex operations
//   - Efficient range scans for directory listings
//   - Concurrent access with proper locking
//
// Thread Safety:
// All operations are protected by a single read-write mutex (mu), making the
// store safe for concurrent access from multiple goroutines. This coarse-grained
// locking is simple and correct, though fine-grained locking could improve
// concurrency for high-throughput scenarios.
//
// Storage Model:
// The store uses a key-value model with namespaced prefixes to organize different
// data types (see keys.go for detailed schema documentation). This approach provides:
//   - No schema conflicts between data types
//   - Efficient point lookups (O(1))
//   - Fast range scans for directory listings and sessions
//   - Self-documenting database structure
//
// File Handle Strategy:
// File handles are generated from filesystem paths, providing deterministic and
// reversible handle generation. This enables:
//   - Importing existing filesystems into DittoFS
//   - Reconstructing metadata from content stores
//   - Debugging with human-readable handles
//   - Stable handles across server restarts
//
// For paths exceeding NFS limits (64 bytes), handles are automatically converted
// to hash-based format with reverse mapping stored in the database.
type BadgerMetadataStore struct {
	// db is the BadgerDB database handle (thread-safe, uses internal MVCC)
	db *badger.DB

	// capabilities stores static filesystem capabilities and limits.
	// These are set at creation time and define what the filesystem supports.
	capabilities metadata.FilesystemCapabilities

	// maxStorageBytes is the maximum total bytes that can be stored.
	// 0 means unlimited (constrained only by available disk space).
	maxStorageBytes uint64

	// maxFiles is the maximum number of files (inodes) that can be created.
	// 0 means unlimited (constrained only by available disk space).
	maxFiles uint64

	// statsCache caches filesystem statistics to avoid expensive database scans.
	// Filesystem statistics require scanning all file entries, which can be slow.
	// This cache stores the result with a timestamp and TTL to serve repeated
	// FSSTAT requests efficiently (macOS Finder calls FSSTAT very frequently).
	statsCache struct {
		stats     metadata.FilesystemStatistics
		hasStats  bool
		timestamp time.Time
		ttl       time.Duration
		mu        sync.RWMutex
	}
}

// BadgerMetadataStoreConfig contains configuration for creating a BadgerDB metadata store.
//
// This structure allows explicit configuration of store capabilities, limits, and
// BadgerDB options at creation time.
type BadgerMetadataStoreConfig struct {
	// DBPath is the directory where BadgerDB will store its files
	// BadgerDB creates multiple files in this directory (value log, LSM tree, etc.)
	DBPath string `mapstructure:"db_path"`

	// Capabilities defines static filesystem capabilities and limits
	Capabilities metadata.FilesystemCapabilities `mapstructure:"capabilities"`

	// MaxStorageBytes is the maximum total bytes that can be stored
	// 0 means unlimited (constrained only by available disk space)
	MaxStorageBytes uint64 `mapstructure:"max_storage_bytes"`

	// MaxFiles is the maximum number of files that can be created
	// 0 means unlimited (constrained only by available disk space)
	MaxFiles uint64 `mapstructure:"max_files"`

	// BadgerOptions allows customization of BadgerDB behavior
	// If nil, sensible defaults are used
	BadgerOptions *badger.Options

	// BlockCacheSizeMB is BadgerDB's block cache size in MB (default: 256)
	// This caches LSM-tree data blocks for faster reads
	BlockCacheSizeMB int64

	// IndexCacheSizeMB is BadgerDB's index cache size in MB (default: 128)
	// This caches LSM-tree indices for faster lookups
	IndexCacheSizeMB int64
}

// NewBadgerMetadataStore creates a new BadgerDB-based metadata store with specified configuration.
//
// The store is initialized with the provided capabilities and limits, which define
// what the filesystem supports and its constraints. BadgerDB is opened at the
// specified path and will create the directory if it doesn't exist.
//
// The returned store is immediately ready for use and safe for concurrent
// access from multiple goroutines.
//
// Context Cancellation:
// This operation respects context cancellation during database initialization.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - config: Configuration including DB path, capabilities, and limits
//
// Returns:
//   - *BadgerMetadataStore: A new store instance ready for use
//   - error: Error if database initialization fails or context is cancelled
//
// Example:
//
//	config := BadgerMetadataStoreConfig{
//	    DBPath: "/var/lib/dittofs/metadata",
//	    Capabilities: metadata.FilesystemCapabilities{
//	        MaxReadSize: 1048576,
//	        MaxFileSize: 1099511627776, // 1TB
//	        // ... other fields
//	    },
//	    MaxStorageBytes: 10 * 1024 * 1024 * 1024, // 10GB
//	    MaxFiles: 100000,
//	}
//	store, err := NewBadgerMetadataStore(ctx, config)
func NewBadgerMetadataStore(ctx context.Context, config BadgerMetadataStoreConfig) (*BadgerMetadataStore, error) {
	// Check context before database operations
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Prepare BadgerDB options
	var opts badger.Options
	if config.BadgerOptions != nil {
		opts = *config.BadgerOptions
	} else {
		// Use sensible defaults for DittoFS metadata workload
		opts = badger.DefaultOptions(config.DBPath)

		// Optimize for metadata workload:
		// - Frequent small reads/writes
		// - Range scans for directory listings
		// - Moderate write amplification acceptable
		// - Large working set from macOS Finder scanning directories
		opts = opts.WithLoggingLevel(badger.WARNING) // Reduce log noise
		opts = opts.WithCompression(options.None)    // Metadata is small, compression overhead not worth it

		// Configure cache sizes (with defaults if not specified)
		blockCacheMB := config.BlockCacheSizeMB
		if blockCacheMB == 0 {
			blockCacheMB = 256 // Default: 256MB
		}
		indexCacheMB := config.IndexCacheSizeMB
		if indexCacheMB == 0 {
			indexCacheMB = 128 // Default: 128MB
		}

		opts = opts.WithBlockCacheSize(blockCacheMB << 20) // Convert MB to bytes
		opts = opts.WithIndexCacheSize(indexCacheMB << 20) // Convert MB to bytes
	}

	// Open BadgerDB
	db, err := badger.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to open BadgerDB at %s: %w", config.DBPath, err)
	}

	store := &BadgerMetadataStore{
		db:              db,
		capabilities:    config.Capabilities,
		maxStorageBytes: config.MaxStorageBytes,
		maxFiles:        config.MaxFiles,
	}

	// Initialize stats cache with a 5-second TTL for responsive updates
	// This prevents expensive database scans on every FSSTAT request while
	// still keeping stats reasonably fresh
	store.statsCache.ttl = 5 * time.Second

	// Initialize singleton keys if they don't exist
	if err := store.initializeSingletons(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to initialize singletons: %w", err)
	}

	return store, nil
}

// NewBadgerMetadataStoreWithDefaults creates a new BadgerDB metadata store with sensible defaults.
//
// This is a convenience constructor that sets up the store with standard capabilities
// and limits suitable for most use cases. See NewMemoryMetadataStoreWithDefaults in
// memory/store.go for the specific default values.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - dbPath: Directory where BadgerDB will store its files
//
// Returns:
//   - *BadgerMetadataStore: A new store instance with default configuration
//   - error: Error if database initialization fails
func NewBadgerMetadataStoreWithDefaults(ctx context.Context, dbPath string) (*BadgerMetadataStore, error) {
	return NewBadgerMetadataStore(ctx, BadgerMetadataStoreConfig{
		DBPath: dbPath,
		Capabilities: metadata.FilesystemCapabilities{
			// Transfer Sizes
			MaxReadSize:        1048576, // 1MB
			PreferredReadSize:  65536,   // 64KB
			MaxWriteSize:       1048576, // 1MB
			PreferredWriteSize: 65536,   // 64KB

			// Limits
			MaxFileSize:      9223372036854775807, // 2^63-1 (practically unlimited)
			MaxFilenameLen:   255,                 // Standard Unix limit
			MaxPathLen:       4096,                // Standard Unix limit
			MaxHardLinkCount: 32767,               // Similar to ext4

			// Features
			SupportsHardLinks:     true,  // We track link counts
			SupportsSymlinks:      true,  // We store symlink targets
			CaseSensitive:         true,  // Keys are case-sensitive
			CasePreserving:        true,  // We store exact filenames
			ChownRestricted:       false, // Allow chown
			SupportsACLs:          false, // No ACL support yet
			SupportsExtendedAttrs: false, // No xattr support yet
			TruncatesLongNames:    true,  // Reject with error

			// Time Resolution
			TimestampResolution: 1, // 1 nanosecond (Go time.Time precision)
		},
		MaxStorageBytes: 0, // Unlimited (reported as available disk space)
		MaxFiles:        0, // Unlimited (reported as 1 million)
	})
}

// initializeSingletons initializes singleton keys if they don't exist.
//
// This creates initial values for:
//   - Server configuration (empty config)
//   - Filesystem capabilities (from config)
//
// These are stored in the database so they persist across restarts.
//
// Thread Safety: Must be called during initialization before concurrent access.
//
// Parameters:
//   - ctx: Context for cancellation
//
// Returns:
//   - error: Error if database operations fail
func (s *BadgerMetadataStore) initializeSingletons(ctx context.Context) error {
	return s.db.Update(func(txn *badger.Txn) error {
		// Initialize server config if it doesn't exist
		_, err := txn.Get(keyServerConfig())
		if err == badger.ErrKeyNotFound {
			// Create default empty config
			config := &metadata.MetadataServerConfig{
				CustomSettings: make(map[string]any),
			}
			configBytes, err := encodeServerConfig(config)
			if err != nil {
				return err
			}
			if err := txn.Set(keyServerConfig(), configBytes); err != nil {
				return fmt.Errorf("failed to initialize server config: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("failed to check server config: %w", err)
		}

		// Initialize filesystem capabilities if they don't exist
		_, err = txn.Get(keyFilesystemCapabilities())
		if err == badger.ErrKeyNotFound {
			capsBytes, err := encodeFilesystemCapabilities(&s.capabilities)
			if err != nil {
				return err
			}
			if err := txn.Set(keyFilesystemCapabilities(), capsBytes); err != nil {
				return fmt.Errorf("failed to initialize capabilities: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("failed to check capabilities: %w", err)
		}

		return nil
	})
}

// Close closes the BadgerDB database and releases all resources.
//
// This should be called when the store is no longer needed, typically during
// server shutdown. After calling Close, the store must not be used.
//
// The close operation waits for all pending transactions to complete and
// flushes all data to disk.
//
// Returns:
//   - error: Error if closing the database fails
func (s *BadgerMetadataStore) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("failed to close BadgerDB: %w", err)
	}

	return nil
}
