package badger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/oklog/ulid/v2"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// BadgerMetadataStore implements metadata.Store using BadgerDB for persistence.
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

	// readCache caches decoded File records for the read hot path so
	// GetFileForRead skips the per-read badger View transaction + File JSON
	// decode. Invalidated after each committed write (see withTransaction).
	readCache fileReadCache

	// gcStop signals the value-log GC goroutine to exit. Closed once by
	// Close() (guarded by gcStopOnce); gcWG waits for the goroutine to
	// drain before Close() shuts the DB so the GC never runs against a
	// closed database.
	gcStop     chan struct{}
	gcStopOnce sync.Once
	gcWG       sync.WaitGroup

	// closeOnce makes the whole Close() idempotent: GC is stopped+waited
	// and the underlying DB is closed exactly once. Second and later calls
	// are a safe no-op returning the first call's result (closeErr).
	closeOnce sync.Once
	closeErr  error

	// capabilities stores static filesystem capabilities and limits.
	// These are set at creation time and define what the filesystem supports.
	// capsMu guards reads/writes of the in-memory copy against the
	// concurrent SetFilesystemCapabilities setters.
	capsMu       sync.RWMutex
	capabilities metadata.FilesystemCapabilities

	// maxStorageBytes is the maximum total bytes that can be stored.
	// 0 means unlimited (constrained only by available disk space).
	maxStorageBytes uint64

	// maxFiles is the maximum number of files (inodes) that can be created.
	// 0 means unlimited (constrained only by available disk space).
	maxFiles uint64

	// shareCache caches decoded ShareOptions for the permission hot path so
	// GetShareOptions (17.4% of server CPU on warm random-read) skips the
	// badger View txn + JSON decode. Invalidated after every committed
	// share-record write; a stale entry is a wrong permission decision.
	shareCache shareReadCache

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

	// lockStore provides lock persistence
	lockStore   *badgerLockStore
	lockStoreMu sync.Mutex

	// clientStore provides NSM client registration persistence
	clientStore   *badgerClientStore
	clientStoreMu sync.Mutex

	// durableStore provides SMB3 durable handle persistence
	durableStore   *badgerDurableStore
	durableStoreMu sync.Mutex

	// recoveryStore provides NFSv4 client-recovery persistence
	recoveryStore   *badgerRecoveryStore
	recoveryStoreMu sync.Mutex

	// usedBytes tracks the total logical bytes used by regular files.
	// Updated atomically on every size-changing operation (create, update, truncate, delete).
	// Initialized from a full file scan on startup.
	usedBytes atomic.Int64

	// userUsage / groupUsage track per-identity usage (bytes + file count) for
	// regular files, keyed by owner uid / gid. In-memory cache mirroring
	// usedBytes, seeded from a full file scan on startup (so it is always
	// reconstructed from the durable file rows — back-compatible with existing
	// dumps). Updated from a transaction's pending per-identity deltas exactly
	// once on successful commit. Guarded by quotaMu.
	quotaMu    sync.Mutex
	userUsage  map[uint32]*metadata.UsageStat
	groupUsage map[uint32]*metadata.UsageStat

	// storeID is the engine-persistent identifier for this store instance,
	// backed by the cfg:store_id key in BadgerDB. Created on first open of
	// a fresh directory with a fresh ULID; read thereafter. Immutable for
	// the life of the instance.
	//
	// Persisting the ULID with the Badger data directory means a control-plane
	// DB reset (which rotates cfg.ID) does NOT cause the engine to report a
	// different identity.
	storeID string

	// relaxedDurability, when set, opens the DB with SyncWrites=false and
	// defers namespace-op fsyncs to the background syncLoop below; durable
	// writes (WithTransaction, SetRollupOffset) call db.Sync() explicitly.
	// When false the DB is opened SyncWrites=true and every commit fsyncs, so
	// WithTransactionRelaxed is indistinguishable from WithTransaction (the
	// pre-#1573 posture). See BadgerMetadataStoreConfig.RelaxedDurability.
	relaxedDurability bool

	// syncStop signals the bounded-lag background syncer to exit; syncWG waits
	// for it in Close(). Only started when relaxedDurability is set. Mirrors the
	// gcStop/gcWG lifecycle so the syncer never runs against a closed DB.
	syncStop     chan struct{}
	syncStopOnce sync.Once
	syncWG       sync.WaitGroup

	// inlineSyncs counts explicit db.Sync() calls on the durable write path
	// (syncIfRelaxed), NOT the background ticker. Tests read it to assert the
	// durable/relaxed classification is wired correctly.
	inlineSyncs atomic.Int64

	// syncLeader coalesces concurrent durable db.Sync() calls (write path +
	// rollup + ticker) into as few fsyncs as possible. See #1573: without it,
	// N concurrent durable commits serialize on badger's single Sync mutex and
	// pay N fsyncs where one would do. Always initialized; only exercised in
	// relaxed mode (strict mode fsyncs on commit, so syncIfRelaxed is a no-op).
	syncLeader *commitLeader
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

	// RelaxedDurability defers the per-transaction fsync for pure-namespace
	// metadata writes (create/remove/rename/mkdir/attr) to a bounded-lag
	// background sync, honoring the same UNSTABLE-style tradeoff the block
	// append-log took in #1584. Data-paired writes — file size on WRITE, the
	// block manifest (DefaultCommitBlock), and the rollup offset — stay
	// synchronous regardless, so this can never resurrect the #588 silent-zeros
	// bug; only a hard crash can lose the last sub-100ms of namespace ops (the
	// op vanishes / reappears, never corrupts). When false (the safe default at
	// the store layer) every commit fsyncs, exactly reproducing pre-#1573
	// behavior. The server product enables it via config (#1573 Wall 1).
	RelaxedDurability bool `mapstructure:"relaxed_durability"`

	// BlockCacheSizeMB is BadgerDB's block cache size in MiB. This caches
	// decompressed LSM-tree data blocks for faster reads. When 0 (unset) the
	// size is resolved from the global config (SetGlobalBadgerCacheDefaults)
	// or, failing that, RAM-relative auto-sizing (autoSizeCacheMB). See
	// cache.go / #1245 Bug D.
	BlockCacheSizeMB int64

	// IndexCacheSizeMB is BadgerDB's index cache size in MiB. This caches
	// LSM-tree block indices for faster key lookups. When 0 (unset) the size
	// is resolved from the global config or RAM-relative auto-sizing — see
	// BlockCacheSizeMB.
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

	// Prepare BadgerDB options. Option construction (including the resolved
	// block/index cache sizing — #1245 Bug D) lives in the pure, testable
	// buildBadgerOptions helper. When config.BadgerOptions is nil the helper
	// applies metadata-workload defaults and resolves the cache sizes with
	// precedence: explicit per-store config > global config > RAM-relative
	// auto-sizing. detectAvailableMemory is indirected for testability.
	//
	// availMem is only consulted on the default-options path (the
	// RAM-relative auto-sizing fallback). On the custom-options path
	// buildBadgerOptions returns config.BadgerOptions verbatim and ignores
	// availMem entirely, so skip the sysinfo probe there.
	var availMem uint64
	if config.BadgerOptions == nil {
		availMem = detectAvailableMemory()
	}
	opts := buildBadgerOptions(config, availMem)

	// Crash-consistency (#583, enforced #588): force SyncWrites=true on
	// every code path — default-options AND custom-options. Default
	// badger.DefaultOptions has SyncWrites=false, which means each
	// committed Update returns as soon as the value lands in the
	// memtable + WAL buffer — NOT after the WAL is fsynced. A `kill -9`
	// (or power loss) between flush boundaries loses every metadata
	// write since the last sync, including rolled-up FileChunk rows
	// and FileAttr.Blocks manifests. Without those rows the engine's
	// read path falls through to the sparse-block zero-fill branch
	// (engine.go:1072 `clear(dest)`), returning silent zeros for files
	// whose CAS chunks are still on disk.
	//
	// Override here so an operator tuning unrelated knobs via BadgerOptions
	// cannot accidentally change the durability posture by inheriting badger's
	// permissive SyncWrites=false default — the posture is decided ONLY by
	// config.RelaxedDurability.
	//
	// Strict (default, RelaxedDurability=false): SyncWrites=true, every commit
	// fsyncs — the #583/#588 posture, unchanged.
	//
	// Relaxed (RelaxedDurability=true, #1573 Wall 1): SyncWrites=false, so
	// namespace-op commits return once the write lands in the memtable/WAL
	// buffer. Durability is re-established two ways: (a) durable writes
	// (WithTransaction, SetRollupOffset) call db.Sync() explicitly after commit
	// — this keeps every DATA-PAIRED write (file size, block manifest, rollup
	// offset) synchronous, so #588 silent-zeros cannot recur; (b) the
	// background syncLoop fsyncs on a bounded interval so an un-barriered
	// namespace op is durable within syncLoopInterval. A hard crash can lose
	// only the last <interval of pure-namespace ops (the op vanishes/reappears,
	// never corrupts) — the same UNSTABLE-style tradeoff #1584 took for the
	// block append-log.
	opts = opts.WithSyncWrites(!config.RelaxedDurability)

	// Open BadgerDB
	db, err := badger.Open(opts)
	if err != nil {
		// BadgerDB takes a directory lock, so this is the failure a second server
		// (or a leftover one that never shut down) hits against the same data dir.
		// Badger's raw "resource temporarily unavailable" is opaque — point the
		// operator straight at the cause and the fix instead.
		if isDirLockErr(err) {
			return nil, fmt.Errorf("metadata store at %s is locked by another process — "+
				"a DittoFS server is almost certainly already running against this data directory. "+
				"Stop it ('dfs stop', or kill the running dfs) before starting another, or point "+
				"this share at a different db_path: %w", config.DBPath, err)
		}
		return nil, fmt.Errorf("failed to open BadgerDB at %s: %w", config.DBPath, err)
	}

	// Bootstrap the engine-persistent store_id before serving requests.
	// ensureStoreID is idempotent — first open writes a fresh ULID,
	// subsequent opens read the existing value.
	sid, err := ensureStoreID(db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ensure store_id: %w", err)
	}

	store := &BadgerMetadataStore{
		db:                db,
		gcStop:            make(chan struct{}),
		capabilities:      config.Capabilities,
		maxStorageBytes:   config.MaxStorageBytes,
		maxFiles:          config.MaxFiles,
		storeID:           sid,
		relaxedDurability: config.RelaxedDurability,
		syncStop:          make(chan struct{}),
	}
	store.syncLeader = newCommitLeader(store.db.Sync)

	// Initialize stats cache with a 5-second TTL for responsive updates
	// This prevents expensive database scans on every FSSTAT request while
	// still keeping stats reasonably fresh
	store.statsCache.ttl = 5 * time.Second

	// Initialize singleton keys if they don't exist
	if err := store.initializeSingletons(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to initialize singletons: %w", err)
	}

	// Initialize the usedBytes counter from a full file scan.
	if err := store.initUsedBytesCounter(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to initialize used bytes counter: %w", err)
	}

	// Start the background value-log GC loop. Badger reclaims value-log
	// space only when RunValueLogGC is called explicitly; without it the
	// value log grows without bound (unbounded disk growth). The loop is
	// stopped in Close().
	store.gcWG.Add(1)
	go store.runValueLogGC()

	// Bounded-lag durability syncer: only needed in relaxed mode, where
	// namespace-op commits do not fsync inline. It caps how long an
	// un-barriered namespace op can sit un-fsynced (worst-case crash-loss
	// window). Strict mode fsyncs every commit, so no syncer runs.
	if store.relaxedDurability {
		store.syncWG.Add(1)
		go store.runDurabilitySync()
	}

	return store, nil
}

// valueLogGCInterval is how often the background loop attempts a Badger
// value-log GC pass. Badger's docs recommend running GC periodically
// (e.g. on a several-minute ticker); this cadence reclaims space without
// adding meaningful background load to the metadata workload.
const valueLogGCInterval = 5 * time.Minute

// valueLogGCDiscardRatio is the fraction of stale data a value-log file
// must contain before Badger will rewrite it. 0.5 is Badger's commonly
// recommended starting point — rewrite files at least half garbage.
const valueLogGCDiscardRatio = 0.5

// isDirLockErr reports whether err is BadgerDB's "directory is already locked"
// failure — the signature of a second server (or a leftover one that never shut
// down) opening the same data directory. Badger's message names the cause
// ("Cannot acquire directory lock ... Another process is using this Badger
// database") before the wrapped EAGAIN errno, so match on that text and not the
// bare "resource temporarily unavailable" (which unrelated failures also carry).
func isDirLockErr(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "cannot acquire directory lock") ||
		strings.Contains(s, "another process is using this badger database")
}

// runValueLogGC periodically reclaims Badger value-log space. On each
// tick it drains all rewritable value-log files (RunValueLogGC returns
// nil after a successful rewrite, so we loop until it reports
// badger.ErrNoRewrite or any other error). The goroutine exits promptly
// when gcStop is closed by Close().
func (s *BadgerMetadataStore) runValueLogGC() {
	defer s.gcWG.Done()

	ticker := time.NewTicker(valueLogGCInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.gcStop:
			return
		case <-ticker.C:
			// Reclaim every rewritable file this cycle. RunValueLogGC
			// rewrites at most one file per call and returns nil on
			// success, so loop until there is nothing left to rewrite.
			for {
				select {
				case <-s.gcStop:
					return
				default:
				}
				if err := s.db.RunValueLogGC(valueLogGCDiscardRatio); err != nil {
					// ErrNoRewrite (nothing to reclaim) is the normal
					// stop condition; any other error (e.g. DB closing)
					// also ends this cycle.
					break
				}
			}
		}
	}
}

// durabilitySyncInterval bounds how long a relaxed (namespace-op) commit can
// sit un-fsynced before the background syncer forces it to disk — i.e. the
// worst-case crash-loss window for pure-namespace ops. 100ms is an order of
// magnitude tighter than ext4's default 5s journal-commit interval.
const durabilitySyncInterval = 100 * time.Millisecond

// runDurabilitySync periodically fsyncs the value log so relaxed-mode
// namespace commits become durable within durabilitySyncInterval even when no
// durable write (which fsyncs inline) happens to follow them. Only started in
// relaxed mode. Exits promptly when syncStop is closed by Close(); a final
// flush is guaranteed by db.Close() itself.
func (s *BadgerMetadataStore) runDurabilitySync() {
	defer s.syncWG.Done()

	ticker := time.NewTicker(durabilitySyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.syncStop:
			return
		case <-ticker.C:
			// A failed periodic sync is not fatal here: the next durable
			// write or db.Close() will retry, and callers that need a hard
			// guarantee go through the inline db.Sync() on the durable path.
			// Route through the leader so the ticker coalesces with concurrent
			// writers instead of contending on badger's Sync mutex (#1573).
			_ = s.syncLeader.Sync(context.Background())
		}
	}
}

// syncIfRelaxed fsyncs the value log when running in relaxed mode, turning a
// just-committed (SyncWrites=false) write durable. In strict mode SyncWrites=true
// already fsynced on commit, so this is a no-op. Callers on the DATA-PAIRED
// path (WithTransaction, SetRollupOffset) use it to keep #588 durability.
func (s *BadgerMetadataStore) syncIfRelaxed() error {
	if !s.relaxedDurability {
		return nil
	}
	s.inlineSyncs.Add(1)
	// Coalesce concurrent durable syncs onto one db.Sync (#1573). Every caller
	// commits its badger txn before reaching here, so a barrier that runs after
	// this enqueue flushes this caller's write — identical durability to a direct
	// s.db.Sync(), minus the redundant fsyncs N concurrent commits would pay.
	return s.syncLeader.Sync(context.Background())
}

// storeIDKey is the BadgerDB key for the engine-persistent store identifier.
// It lives under the existing "cfg:" singleton-config prefix so it shares a
// namespace with server config and filesystem capabilities.
const storeIDKey = prefixConfig + "store_id"

// ensureStoreID reads the persistent engine store_id from the cfg:store_id
// key, creating it with a fresh ULID on first open. Safe to call on every
// open — idempotent after bootstrap.
func ensureStoreID(db *badger.DB) (string, error) {
	var existing string
	err := db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(storeIDKey))
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return item.Value(func(v []byte) error {
			existing = string(v)
			return nil
		})
	})
	if err != nil {
		return "", fmt.Errorf("read %s: %w", storeIDKey, err)
	}
	if existing != "" {
		return existing, nil
	}
	fresh := ulid.Make().String()
	if err := db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(storeIDKey), []byte(fresh))
	}); err != nil {
		return "", fmt.Errorf("write %s: %w", storeIDKey, err)
	}
	return fresh, nil
}

// GetUsedBytes returns the current total logical bytes used by regular files.
// This is an O(1) atomic read, safe for concurrent access without locks.
func (s *BadgerMetadataStore) GetUsedBytes() int64 {
	return s.usedBytes.Load()
}

// initUsedBytesCounter scans all file entries once at startup to initialize the
// store-wide atomic counter and the per-identity usage cache (userUsage /
// groupUsage). Both are reconstructed from the durable file rows, so a store
// opened from an existing dump (with no separately persisted counters) is always
// seeded correctly — back-compatible by construction.
func (s *BadgerMetadataStore) initUsedBytesCounter() error {
	var totalUsed int64
	userUsage := make(map[uint32]*metadata.UsageStat)
	groupUsage := make(map[uint32]*metadata.UsageStat)

	addUsage := func(m map[uint32]*metadata.UsageStat, id uint32, bytes int64) {
		u := m[id]
		if u == nil {
			u = &metadata.UsageStat{}
			m[id] = u
		}
		u.Bytes += bytes
		u.Files++
	}

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(prefixFile)
		opts.PrefetchValues = true

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			err := item.Value(func(val []byte) error {
				file, err := decodeFile(val)
				if err != nil {
					return nil // Skip corrupted entries
				}
				if file.Type == metadata.FileTypeRegular {
					totalUsed += int64(file.Size)
					addUsage(userUsage, file.UID, int64(file.Size))
					addUsage(groupUsage, file.GID, int64(file.Size))
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	s.usedBytes.Store(totalUsed)
	s.quotaMu.Lock()
	s.userUsage = userUsage
	s.groupUsage = groupUsage
	s.quotaMu.Unlock()
	return nil
}

// GetQuotaUsage returns per-identity usage for the given scope and id.
// O(1) cache read under quotaMu. A missing key returns a zero UsageStat.
func (s *BadgerMetadataStore) GetQuotaUsage(scope metadata.QuotaScope, id uint32) (metadata.UsageStat, error) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	m := s.userUsage
	if scope == metadata.QuotaScopeGroup {
		m = s.groupUsage
	}
	if u, ok := m[id]; ok {
		return *u, nil
	}
	return metadata.UsageStat{}, nil
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
	return NewBadgerMetadataStore(ctx, defaultStoreConfig(dbPath))
}

// NewBadgerMetadataStoreWithDefaultsAndCaches is NewBadgerMetadataStoreWithDefaults
// with explicit Badger block/index cache sizes (in MiB). A zero size for either
// dimension defers that cache to the global config / RAM-relative auto-sizing
// (see cache.go / #1245 Bug D). Used by the per-store config-map path so an
// operator can pin caches on a single metadata store via its config keys
// (block_cache_mb / index_cache_mb).
func NewBadgerMetadataStoreWithDefaultsAndCaches(ctx context.Context, dbPath string, blockCacheMB, indexCacheMB int64, relaxedDurability bool) (*BadgerMetadataStore, error) {
	cfg := defaultStoreConfig(dbPath)
	cfg.BlockCacheSizeMB = blockCacheMB
	cfg.IndexCacheSizeMB = indexCacheMB
	cfg.RelaxedDurability = relaxedDurability
	return NewBadgerMetadataStore(ctx, cfg)
}

// defaultStoreConfig returns the standard BadgerMetadataStoreConfig (capabilities
// and limits) for the given path, with cache sizes left at 0 (auto-sized).
func defaultStoreConfig(dbPath string) BadgerMetadataStoreConfig {
	return BadgerMetadataStoreConfig{
		DBPath: dbPath,
		Capabilities: metadata.FilesystemCapabilities{
			// Transfer Sizes
			MaxReadSize:        1048576, // 1MB
			PreferredReadSize:  1048576, // 1MB — matches Linux knfsd default; reduces NFS round-trips per block
			MaxWriteSize:       1048576, // 1MB
			PreferredWriteSize: 1048576, // 1MB

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
			SupportsExtendedAttrs: true,  // EAs persist in the FileAttr JSON blob
			TruncatesLongNames:    true,  // Reject with error

			// Time Resolution
			TimestampResolution: 1, // 1 nanosecond (Go time.Time precision)
		},
		MaxStorageBytes: 0, // Unlimited (reported as available disk space)
		MaxFiles:        0, // Unlimited (reported as 1 million)
	}
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
			caps := s.loadCapabilities()
			capsBytes, err := encodeFilesystemCapabilities(&caps)
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
// Close is idempotent: the GC goroutine is stopped and waited, and the
// underlying BadgerDB is closed, exactly once. A second (or later) call is a
// safe no-op that returns the first call's result without touching the DB
// again — badger's db.Close() is not safe to call twice.
//
// Returns:
//   - error: Error if closing the database fails (on the first call)
func (s *BadgerMetadataStore) Close() error {
	s.closeOnce.Do(func() {
		// Stop the value-log GC goroutine and wait for it to drain before
		// closing the DB, so no GC pass runs against a closed database.
		// gcStopOnce guards the channel close in case the GC stop is ever
		// signalled from another path.
		s.gcStopOnce.Do(func() {
			close(s.gcStop)
		})
		s.gcWG.Wait()

		// Stop the bounded-lag durability syncer (relaxed mode only) before
		// closing the DB. syncStopOnce is a no-op if the syncer never started.
		s.syncStopOnce.Do(func() {
			close(s.syncStop)
		})
		s.syncWG.Wait()

		// Record a clean-shutdown marker LAST, after the GC goroutine has
		// drained and before closing the DB, so the lock-recovery boot path can
		// distinguish a graceful drain from a kill -9 / crash. Close is the
		// single graceful teardown site for the store. A persist failure is
		// intentionally swallowed: leaving the marker unwritten makes the next
		// boot conservatively treat the start as unclean and enter grace, which
		// is the fail-safe direction.
		_ = s.SetCleanShutdown(context.Background(), true)

		if err := s.db.Close(); err != nil {
			s.closeErr = fmt.Errorf("failed to close BadgerDB: %w", err)
		}
	})

	return s.closeErr
}

// BadgerOptions returns the badger.Options the underlying DB was opened with.
// Exposed for diagnostics and tests (e.g. asserting the resolved block/index
// cache sizes were threaded into the open — #1245 Bug D). The returned value is
// a copy; mutating it does not affect the live DB.
func (s *BadgerMetadataStore) BadgerOptions() badger.Options { return s.db.Opts() }

// GetStoreID returns the Badger-persistent store identifier (stored at key
// cfg:store_id). Stable across restarts — the ULID is written once on first
// open of a fresh directory and read on every subsequent open. Immutable
// for the life of the instance.
func (s *BadgerMetadataStore) GetStoreID() string { return s.storeID }

// Compile-time assertion: the Badger engine exposes GetStoreID.
var _ interface{ GetStoreID() string } = (*BadgerMetadataStore)(nil)
