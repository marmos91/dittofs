package badger

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
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
	// mu protects all fields in this struct for concurrent access.
	// Operations acquire read locks for queries and write locks for mutations.
	mu sync.RWMutex

	// db is the BadgerDB database handle
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

	// readdirCache caches directory listings (LRU + TTL).
	readdirCache struct {
		enabled      bool
		ttl          time.Duration
		maxEntries   int
		invalidOnMod bool
		cache        map[string]*readdirCacheEntry
		lruList      *list.List
		mu           sync.RWMutex
		hits         uint64
		misses       uint64
	}

	// lookupCache caches lookup results (LRU + TTL).
	lookupCache struct {
		enabled      bool
		ttl          time.Duration
		maxEntries   int
		invalidOnMod bool
		cache        map[string]*lookupCacheEntry
		lruList      *list.List
		mu           sync.RWMutex
		hits         uint64
		misses       uint64
	}

	// getfileCache caches file attributes by handle (LRU + TTL).
	getfileCache struct {
		enabled      bool
		ttl          time.Duration
		maxEntries   int
		invalidOnMod bool
		cache        map[string]*getfileCacheEntry
		lruList      *list.List
		mu           sync.RWMutex
		hits         uint64
		misses       uint64
	}

	// shareNameCache caches share names by handle (LRU + TTL).
	// Share names are immutable for a given handle, making them ideal for caching.
	// This avoids expensive DB lookups on every NFS operation that needs auth context.
	shareNameCache struct {
		enabled    bool
		ttl        time.Duration
		maxEntries int
		cache      map[string]*shareNameCacheEntry
		lruList    *list.List
		mu         sync.RWMutex
		hits       uint64
		misses     uint64
	}
}

// readdirCacheEntry stores cached directory listing.
type readdirCacheEntry struct {
	children  []metadata.FileHandle
	attrs     []*metadata.FileAttr
	names     []string
	timestamp time.Time
	lruNode   *list.Element
}

// lookupCacheEntry stores cached lookup result.
type lookupCacheEntry struct {
	handle    metadata.FileHandle
	attr      *metadata.FileAttr
	timestamp time.Time
	lruNode   *list.Element
}

// getfileCacheEntry stores cached file attributes.
type getfileCacheEntry struct {
	attr      *metadata.FileAttr
	timestamp time.Time
	lruNode   *list.Element
}

// shareNameCacheEntry stores cached share name.
type shareNameCacheEntry struct {
	shareName string
	timestamp time.Time
	lruNode   *list.Element
}

// BadgerMetadataStoreConfig contains configuration for creating a BadgerDB metadata store.
//
// This structure allows explicit configuration of store capabilities, limits, and
// BadgerDB options at creation time.
type BadgerMetadataStoreConfig struct {
	// DBPath is the directory where BadgerDB will store its files
	// BadgerDB creates multiple files in this directory (value log, LSM tree, etc.)
	DBPath string

	// Capabilities defines static filesystem capabilities and limits
	Capabilities metadata.FilesystemCapabilities

	// MaxStorageBytes is the maximum total bytes that can be stored
	// 0 means unlimited (constrained only by available disk space)
	MaxStorageBytes uint64

	// MaxFiles is the maximum number of files that can be created
	// 0 means unlimited (constrained only by available disk space)
	MaxFiles uint64

	// BadgerOptions allows customization of BadgerDB behavior
	// If nil, sensible defaults are used
	BadgerOptions *badger.Options

	// CacheEnabled enables metadata caching (readdir + lookup)
	CacheEnabled bool

	// CacheTTL is cache entry lifetime (default: 5s)
	CacheTTL time.Duration

	// CacheMaxEntries limits cache size (default: 1000 for readdir, 10000 for lookup)
	CacheMaxEntries int

	// CacheInvalidateOnWrite clears cache on modifications (default: true)
	CacheInvalidateOnWrite bool

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

	// Initialize caches if enabled
	if config.CacheEnabled {
		// Common settings
		ttl := config.CacheTTL
		if ttl == 0 {
			ttl = 5 * time.Second
		}
		invalidOnMod := config.CacheInvalidateOnWrite

		// ReadDir cache (smaller - one entry per directory)
		store.readdirCache.enabled = true
		store.readdirCache.ttl = ttl
		store.readdirCache.maxEntries = config.CacheMaxEntries
		if store.readdirCache.maxEntries == 0 {
			store.readdirCache.maxEntries = 1000
		}
		store.readdirCache.invalidOnMod = invalidOnMod
		store.readdirCache.cache = make(map[string]*readdirCacheEntry)
		store.readdirCache.lruList = list.New()

		// Lookup cache (larger - one entry per file)
		// Default: 10x readdir cache size (10000 entries), or 10x configured value
		store.lookupCache.enabled = true
		store.lookupCache.ttl = ttl
		if config.CacheMaxEntries == 0 {
			store.lookupCache.maxEntries = 10000 // Default
		} else {
			store.lookupCache.maxEntries = config.CacheMaxEntries * 10
		}
		store.lookupCache.invalidOnMod = invalidOnMod
		store.lookupCache.cache = make(map[string]*lookupCacheEntry)
		store.lookupCache.lruList = list.New()

		// GetFile cache (same size as lookup - one entry per file)
		// Default: 10000 entries, or 10x configured value
		store.getfileCache.enabled = true
		store.getfileCache.ttl = ttl
		if config.CacheMaxEntries == 0 {
			store.getfileCache.maxEntries = 10000 // Default
		} else {
			store.getfileCache.maxEntries = config.CacheMaxEntries * 10
		}
		store.getfileCache.invalidOnMod = invalidOnMod
		store.getfileCache.cache = make(map[string]*getfileCacheEntry)
		store.getfileCache.lruList = list.New()

		// ShareName cache (same size as getfile - one entry per file)
		// Share names are immutable and frequently accessed on every NFS operation
		// Default: 10000 entries, or 10x configured value
		store.shareNameCache.enabled = true
		store.shareNameCache.ttl = ttl
		if config.CacheMaxEntries == 0 {
			store.shareNameCache.maxEntries = 10000 // Default
		} else {
			store.shareNameCache.maxEntries = config.CacheMaxEntries * 10
		}
		store.shareNameCache.cache = make(map[string]*shareNameCacheEntry)
		store.shareNameCache.lruList = list.New()

		logger.Info("Metadata cache enabled: ttl=%v readdir_max=%d lookup_max=%d getfile_max=%d sharename_max=%d invalidate_on_write=%v",
			ttl, store.readdirCache.maxEntries, store.lookupCache.maxEntries, store.getfileCache.maxEntries, store.shareNameCache.maxEntries, invalidOnMod)
	}

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

// invalidateStatsCache invalidates the filesystem statistics cache.
//
// This should be called after operations that modify file count or total size:
//   - Creating files/directories/symlinks
//   - Removing files/directories
//   - Writing to files (changes size)
//   - Truncating files (changes size)
//
// The cache will be recomputed on the next GetFilesystemStatistics call.
//
// Thread Safety: Safe for concurrent use.
func (s *BadgerMetadataStore) invalidateStatsCache() {
	s.statsCache.mu.Lock()
	s.statsCache.hasStats = false
	s.statsCache.mu.Unlock()
}

// readdirCacheKey generates cache key for directory listing.
func readdirCacheKey(handle metadata.FileHandle) string {
	return string(handle)
}

// lookupCacheKey generates cache key for lookup operation.
func lookupCacheKey(parentHandle metadata.FileHandle, name string) string {
	return string(parentHandle) + ":" + name
}

// getReaddirCached retrieves cached directory listing if valid.
func (s *BadgerMetadataStore) getReaddirCached(handle metadata.FileHandle) *readdirCacheEntry {
	if !s.readdirCache.enabled {
		return nil
	}

	s.readdirCache.mu.RLock()
	defer s.readdirCache.mu.RUnlock()

	key := readdirCacheKey(handle)
	entry, exists := s.readdirCache.cache[key]
	if !exists {
		return nil
	}

	// Check TTL
	if time.Since(entry.timestamp) > s.readdirCache.ttl {
		return nil
	}

	// Update LRU
	s.readdirCache.lruList.MoveToFront(entry.lruNode)
	return entry
}

// putReaddirCached stores directory listing in cache.
func (s *BadgerMetadataStore) putReaddirCached(handle metadata.FileHandle, children []metadata.FileHandle, attrs []*metadata.FileAttr, names []string) {
	if !s.readdirCache.enabled {
		return
	}

	s.readdirCache.mu.Lock()
	defer s.readdirCache.mu.Unlock()

	key := readdirCacheKey(handle)

	// Update existing entry
	if existing, exists := s.readdirCache.cache[key]; exists {
		existing.children = children
		existing.attrs = attrs
		existing.names = names
		existing.timestamp = time.Now()
		s.readdirCache.lruList.MoveToFront(existing.lruNode)
		return
	}

	// Evict if full
	if len(s.readdirCache.cache) >= s.readdirCache.maxEntries {
		s.evictOldestReaddir()
	}

	// Create new entry
	entry := &readdirCacheEntry{
		children:  children,
		attrs:     attrs,
		names:     names,
		timestamp: time.Now(),
	}
	entry.lruNode = s.readdirCache.lruList.PushFront(key)
	s.readdirCache.cache[key] = entry
}

// evictOldestReaddir removes LRU entry from readdir cache.
func (s *BadgerMetadataStore) evictOldestReaddir() {
	if s.readdirCache.lruList.Len() == 0 {
		return
	}

	oldest := s.readdirCache.lruList.Back()
	if oldest == nil {
		return
	}
	s.readdirCache.lruList.Remove(oldest)

	key := oldest.Value.(string)
	delete(s.readdirCache.cache, key)
}

// getLookupCached retrieves cached lookup result if valid.
func (s *BadgerMetadataStore) getLookupCached(parentHandle metadata.FileHandle, name string) *lookupCacheEntry {
	if !s.lookupCache.enabled {
		return nil
	}

	s.lookupCache.mu.RLock()
	defer s.lookupCache.mu.RUnlock()

	key := lookupCacheKey(parentHandle, name)
	entry, exists := s.lookupCache.cache[key]
	if !exists {
		return nil
	}

	// Check TTL
	if time.Since(entry.timestamp) > s.lookupCache.ttl {
		return nil
	}

	// Update LRU
	s.lookupCache.lruList.MoveToFront(entry.lruNode)
	return entry
}

// putLookupCached stores lookup result in cache.
func (s *BadgerMetadataStore) putLookupCached(parentHandle metadata.FileHandle, name string, handle metadata.FileHandle, attr *metadata.FileAttr) {
	if !s.lookupCache.enabled {
		return
	}

	s.lookupCache.mu.Lock()
	defer s.lookupCache.mu.Unlock()

	key := lookupCacheKey(parentHandle, name)

	// Update existing entry
	if existing, exists := s.lookupCache.cache[key]; exists {
		existing.handle = handle
		existing.attr = attr
		existing.timestamp = time.Now()
		s.lookupCache.lruList.MoveToFront(existing.lruNode)
		return
	}

	// Evict if full
	if len(s.lookupCache.cache) >= s.lookupCache.maxEntries {
		s.evictOldestLookup()
	}

	// Create new entry
	entry := &lookupCacheEntry{
		handle:    handle,
		attr:      attr,
		timestamp: time.Now(),
	}
	entry.lruNode = s.lookupCache.lruList.PushFront(key)
	s.lookupCache.cache[key] = entry
}

// evictOldestLookup removes LRU entry from lookup cache.
func (s *BadgerMetadataStore) evictOldestLookup() {
	if s.lookupCache.lruList.Len() == 0 {
		return
	}

	oldest := s.lookupCache.lruList.Back()
	if oldest == nil {
		return
	}
	s.lookupCache.lruList.Remove(oldest)

	key := oldest.Value.(string)
	delete(s.lookupCache.cache, key)
}

// invalidateReaddir removes directory from readdir cache.
func (s *BadgerMetadataStore) invalidateReaddir(handle metadata.FileHandle) {
	if !s.readdirCache.enabled || !s.readdirCache.invalidOnMod {
		return
	}

	s.readdirCache.mu.Lock()
	defer s.readdirCache.mu.Unlock()

	key := readdirCacheKey(handle)
	entry, exists := s.readdirCache.cache[key]
	if !exists {
		return
	}

	s.readdirCache.lruList.Remove(entry.lruNode)
	delete(s.readdirCache.cache, key)
}

// invalidateLookup removes all lookups for a directory from cache.
func (s *BadgerMetadataStore) invalidateLookup(parentHandle metadata.FileHandle) {
	if !s.lookupCache.enabled || !s.lookupCache.invalidOnMod {
		return
	}

	s.lookupCache.mu.Lock()
	defer s.lookupCache.mu.Unlock()

	// Remove all lookups with this parent
	prefix := string(parentHandle) + ":"
	for key, entry := range s.lookupCache.cache {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			s.lookupCache.lruList.Remove(entry.lruNode)
			delete(s.lookupCache.cache, key)
		}
	}
}

// getGetfileCached retrieves cached file attributes by handle.
func (s *BadgerMetadataStore) getGetfileCached(handle metadata.FileHandle) *getfileCacheEntry {
	if !s.getfileCache.enabled {
		return nil
	}

	s.getfileCache.mu.RLock()
	defer s.getfileCache.mu.RUnlock()

	entry, exists := s.getfileCache.cache[string(handle)]
	if !exists {
		return nil
	}

	// Check TTL
	if time.Since(entry.timestamp) > s.getfileCache.ttl {
		return nil
	}

	// Update LRU
	s.getfileCache.lruList.MoveToFront(entry.lruNode)

	return entry
}

// putGetfileCached stores file attributes in cache.
func (s *BadgerMetadataStore) putGetfileCached(handle metadata.FileHandle, attr *metadata.FileAttr) {
	if !s.getfileCache.enabled {
		return
	}

	s.getfileCache.mu.Lock()
	defer s.getfileCache.mu.Unlock()

	key := string(handle)

	// Update existing entry
	if existing, exists := s.getfileCache.cache[key]; exists {
		existing.attr = attr
		existing.timestamp = time.Now()
		s.getfileCache.lruList.MoveToFront(existing.lruNode)
		return
	}

	// Evict if full
	if len(s.getfileCache.cache) >= s.getfileCache.maxEntries {
		s.evictOldestGetfile()
	}

	// Create new entry
	entry := &getfileCacheEntry{
		attr:      attr,
		timestamp: time.Now(),
	}
	entry.lruNode = s.getfileCache.lruList.PushFront(key)
	s.getfileCache.cache[key] = entry
}

// evictOldestGetfile removes the least recently used getfile cache entry.
func (s *BadgerMetadataStore) evictOldestGetfile() {
	if s.getfileCache.lruList.Len() == 0 {
		return
	}

	oldest := s.getfileCache.lruList.Back()
	if oldest != nil {
		key := oldest.Value.(string)
		delete(s.getfileCache.cache, key)
		s.getfileCache.lruList.Remove(oldest)
	}
}

// invalidateGetfile invalidates cached file attributes for a specific handle.
func (s *BadgerMetadataStore) invalidateGetfile(handle metadata.FileHandle) {
	if !s.getfileCache.enabled || !s.getfileCache.invalidOnMod {
		return
	}

	s.getfileCache.mu.Lock()
	defer s.getfileCache.mu.Unlock()

	key := string(handle)
	if entry, exists := s.getfileCache.cache[key]; exists {
		s.getfileCache.lruList.Remove(entry.lruNode)
		delete(s.getfileCache.cache, key)
	}
}

// getShareNameCached retrieves cached share name if valid.
func (s *BadgerMetadataStore) getShareNameCached(handle metadata.FileHandle) *shareNameCacheEntry {
	if !s.shareNameCache.enabled {
		return nil
	}

	s.shareNameCache.mu.RLock()
	defer s.shareNameCache.mu.RUnlock()

	entry, exists := s.shareNameCache.cache[string(handle)]
	if !exists {
		return nil
	}

	// Check TTL
	if time.Since(entry.timestamp) > s.shareNameCache.ttl {
		return nil
	}

	// Update LRU
	s.shareNameCache.lruList.MoveToFront(entry.lruNode)

	return entry
}

// putShareNameCached stores share name in cache.
func (s *BadgerMetadataStore) putShareNameCached(handle metadata.FileHandle, shareName string) {
	if !s.shareNameCache.enabled {
		return
	}

	s.shareNameCache.mu.Lock()
	defer s.shareNameCache.mu.Unlock()

	key := string(handle)

	// Update existing entry
	if existing, exists := s.shareNameCache.cache[key]; exists {
		existing.shareName = shareName
		existing.timestamp = time.Now()
		s.shareNameCache.lruList.MoveToFront(existing.lruNode)
		return
	}

	// Evict if full
	if len(s.shareNameCache.cache) >= s.shareNameCache.maxEntries {
		s.evictOldestShareName()
	}

	// Create new entry
	entry := &shareNameCacheEntry{
		shareName: shareName,
		timestamp: time.Now(),
	}
	entry.lruNode = s.shareNameCache.lruList.PushFront(key)
	s.shareNameCache.cache[key] = entry
}

// evictOldestShareName removes the least recently used sharename cache entry.
func (s *BadgerMetadataStore) evictOldestShareName() {
	if s.shareNameCache.lruList.Len() == 0 {
		return
	}

	oldest := s.shareNameCache.lruList.Back()
	if oldest != nil {
		key := oldest.Value.(string)
		delete(s.shareNameCache.cache, key)
		s.shareNameCache.lruList.Remove(oldest)
	}
}

// invalidateShareName invalidates cached share name for a specific handle.
func (s *BadgerMetadataStore) invalidateShareName(handle metadata.FileHandle) {
	if !s.shareNameCache.enabled {
		return
	}

	s.shareNameCache.mu.Lock()
	defer s.shareNameCache.mu.Unlock()

	key := string(handle)
	if entry, exists := s.shareNameCache.cache[key]; exists {
		s.shareNameCache.lruList.Remove(entry.lruNode)
		delete(s.shareNameCache.cache, key)
	}
}

// invalidateDirectory invalidates all caches for a directory (called on modifications).
func (s *BadgerMetadataStore) invalidateDirectory(handle metadata.FileHandle) {
	s.invalidateReaddir(handle)
	s.invalidateLookup(handle)
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
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.db.Close(); err != nil {
		return fmt.Errorf("failed to close BadgerDB: %w", err)
	}

	return nil
}

// lookupHashedHandlePath retrieves the original path for a hash-based file handle.
//
// This is used internally when a file handle was generated using hash-based format
// (because the path exceeded 64 bytes). The reverse mapping is looked up from the
// database.
//
// Thread Safety: Safe for concurrent use with appropriate locking.
//
// Parameters:
//   - handle: The hash-based file handle
//
// Returns:
//   - string: The original "shareName:fullPath" string
//   - error: Error if handle not found or lookup fails
func (s *BadgerMetadataStore) lookupHashedHandlePath(handle metadata.FileHandle) (string, error) {
	var path string

	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(keyHandleMapping(handle))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "handle mapping not found for hashed handle",
			}
		}
		if err != nil {
			return err
		}

		return item.Value(func(val []byte) error {
			path = string(val)
			return nil
		})
	})

	return path, err
}

// storeHashedHandleMapping stores the reverse mapping for a hash-based file handle.
//
// This is called when a file handle is generated using hash-based format to maintain
// the reverse mapping needed for path reconstruction.
//
// Thread Safety: Must be called within a transaction.
//
// Parameters:
//   - txn: BadgerDB transaction
//   - handle: The hash-based file handle
//   - shareName: The share name
//   - fullPath: The full path within the share
//
// Returns:
//   - error: Error if storing the mapping fails
func storeHashedHandleMapping(txn *badger.Txn, handle metadata.FileHandle, shareName, fullPath string) error {
	mapping := shareName + ":" + fullPath
	return txn.Set(keyHandleMapping(handle), []byte(mapping))
}
