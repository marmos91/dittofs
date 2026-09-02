package badger

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/gencache"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/sharecache"
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
//   - UUID file handles, stable across renames and restarts
//   - ACID transactions for complex operations
//   - Efficient range scans for directory listings
//
// Thread Safety:
// The store is safe for concurrent use from multiple goroutines, but it holds
// no store-wide mutex. The metadata data path is serialized by BadgerDB's own
// MVCC transactions: conflicting writers fail to commit and are retried by
// withTransaction. In-memory side state is guarded per subsystem rather than
// globally: a mutex each for capabilities, the lazily built lock / client /
// durable / recovery sub-stores, quotas, and the stats cache, while the read /
// parent / dirent / share caches are lock-free (sync.Map plus generation
// counters).
//
// Storage Model:
// The store uses a key-value model with namespaced prefixes to organize different
// data types (see encoding.go for detailed schema documentation). This approach provides:
//   - No schema conflicts between data types
//   - Efficient point lookups (O(1))
//   - Fast range scans for directory listings and sessions
//   - Self-documenting database structure
//
// File Handle Strategy:
// GenerateHandle ignores the path it is given and mints a fresh random UUID
// handle scoped to the share, so a handle is independent of the name a file is
// reachable under and survives renames. The handle layout is owned by
// metadata.EncodeShareHandle; the UUID it carries is what the per-file key
// namespaces in encoding.go are indexed by.
type BadgerMetadataStore struct {
	// db is the BadgerDB database handle (thread-safe, uses internal MVCC)
	db *badger.DB

	// readCache caches decoded File records for the read hot path so
	// GetFileForRead skips the per-read badger View transaction + File JSON
	// decode. That decode inlines the full ChunkRef list — measured ~800 µs for a
	// 1 GiB (≈1024-chunk) file — and the per-read badger View transaction is the
	// top mutex contender under concurrent reads (server pprof, #1169). A
	// random-read fleet hammering one file re-decoded that blob on every 4 KiB
	// read; the cache collapses it to one decode per file. Invalidated after each
	// committed write (see withTransaction).
	readCache gencache.Cache[*metadata.File]

	// parentCache caches decoded parent-directory File records (WITH derived
	// Path) for the create hot path so repeated creates in one directory skip the
	// per-create badger View txn + decode + parent-edge path walk (#1735). Kept
	// SEPARATE from readCache because it holds path-carrying entries, which must
	// never pollute the shared readCache (whose consumers derive Path fresh,
	// #1166). Invalidated in lockstep with readCache on the parent's own mutation
	// (parentID in dirtyFiles) — see GetFileForCreate and withTransaction.
	parentCache gencache.Cache[*metadata.File]

	// direntCache caches (parentID,name) -> childHandle|ABSENT for the
	// pre-transaction existence check on the create path and LOOKUP (#1735).
	// Populate-after-commit, generation-guarded; NEVER consulted for the in-txn
	// TOCTOU recheck. Invalidated after each committed SetChild/DeleteChild.
	direntCache gencache.Cache[direntEntry]

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
	shareCache sharecache.Cache

	// lockStore provides lock persistence
	lockStore *badgerLockStore

	// clientStore provides NSM client registration persistence
	clientStore *badgerClientStore

	// durableStore provides SMB3 durable handle persistence
	durableStore *badgerDurableStore

	// recoveryStore provides NFSv4 client-recovery persistence
	recoveryStore *badgerRecoveryStore

	// quota tracks per-identity usage (bytes + file count) for regular files,
	// keyed by owner uid / gid. In-memory cache mirroring usedBytes, seeded from
	// a full file scan on startup (so it is always reconstructed from the durable
	// file rows — back-compatible with existing dumps). Updated from a
	// transaction's pending per-identity deltas exactly once on successful
	// commit. Guarded by quotaMu.
	quotaMu sync.Mutex
	quota   *basestore.QuotaCache

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
	// writes (WithTransaction) call db.Sync() explicitly.
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

	// txnConflicts counts SSI ErrConflict aborts observed by the retry loops
	// (withTransaction and updateWithConflictRetry), one per retried attempt.
	// It is a pure contention fingerprint:
	// transactions that touch disjoint keys never bump it, so concurrent writers
	// sharing a hot key (e.g. a parent inode) are the only thing that drives it
	// up. Tests read it to assert a workload stays conflict-free.
	txnConflicts atomic.Int64
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

	// RelaxedDurability defers the per-transaction fsync to a bounded-lag
	// background sync, honoring the same UNSTABLE-style tradeoff the block
	// append-log took in #1584. It covers namespace/attr writes
	// (create/remove/rename/mkdir/attr) and, since #1687, the deferrable file-size
	// commit on UNSTABLE WRITE / COMMIT / SMB inline WRITE. The block manifest
	// (DefaultCommitBlock) and the rollup offset still commit synchronously.
	//
	// A relaxed file-size commit no longer risks #588 silent-zeros: the byte data
	// is already fsync'd into the local journal before the WRITE is ACK'd, and on
	// share start reconcileMetadataSizeFromJournal grows metadata.Size up to the
	// journal's durable high-water mark (max-only, never shrinks). So a crash
	// between a relaxed size commit and its background fsync cannot truncate ACK'd
	// data — the reconcile restores the size. Durability-critical paths (FILE_SYNC
	// WRITE, SMB CLOSE/FLUSH, shutdown) still pass durable=true and fsync inline.
	// When false (the safe default at the store layer) every commit fsyncs,
	// exactly reproducing pre-#1573 behavior. The server product enables it via
	// config (#1573 Wall 1).
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
	// (WithTransaction) call db.Sync() explicitly after commit
	// — this keeps every DATA-PAIRED write (file size, block manifest)
	// synchronous, so #588 silent-zeros cannot recur; (b) the
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

	// Guard the format before anything reads or writes through the database: a
	// format this build does not understand must fail the open, not surface as
	// missing data once the store is already serving.
	if err := ensureFormatVersion(db); err != nil {
		_ = db.Close()
		return nil, err
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
		quota:             basestore.NewQuotaCache(),
	}
	// Bound the hot-path caches. Set before first use and never written again;
	// the share cache stays unbounded because shares are few (see sharecache).
	store.readCache.Cap = fileReadCacheCap
	store.parentCache.Cap = fileReadCacheCap
	store.direntCache.Cap = direntCacheCap

	// The substores derive only from db, which is never reassigned, so bind
	// them once here.
	store.lockStore = newBadgerLockStore(db)
	store.clientStore = newBadgerClientStore(db)
	store.durableStore = newBadgerDurableStore(db)
	store.recoveryStore = newBadgerRecoveryStore(db)

	// Initialize singleton keys if they don't exist
	if err := store.initializeSingletons(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to initialize singletons: %w", err)
	}

	// Initialize the usedBytes counter from a full file scan, which also writes
	// the pl: index when this store's rows predate it — one decode pass, not two.
	if err := store.initUsedBytesAndPayloadIndex(); err != nil {
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
			_ = s.db.Sync()
		}
	}
}

// syncIfRelaxed fsyncs the value log when running in relaxed mode, turning a
// just-committed (SyncWrites=false) write durable. In strict mode SyncWrites=true
// already fsynced on commit, so this is a no-op. Callers on the DATA-PAIRED
// path (WithTransaction) use it to keep #588 durability.
func (s *BadgerMetadataStore) syncIfRelaxed() error {
	if !s.relaxedDurability {
		return nil
	}
	s.inlineSyncs.Add(1)
	return s.db.Sync()
}

// storeFormatVersion is the on-disk format version this build writes and can
// read. Bump it in any release that moves where existing data lives — a field
// out of a record into a sibling key, a renamed prefix, a retired key. An older
// binary decodes the record it still recognizes, finds nothing where the moved
// data used to be, and serves a file with the right size and no content.
//
// Adding a NEW key or a new self-describing record field needs no bump — an
// older binary skips what it does not know and loses nothing it had.
const storeFormatVersion uint32 = 1

// formatVersionKey is the BadgerDB key holding storeFormatVersion.
const formatVersionKey = prefixFormat + "store"

// ensureFormatVersion refuses to open a database a newer release wrote, and
// stamps the current version otherwise. An unstamped database predates stamping
// and is adopted, so every later open is guarded; a stamp below the current
// version is raised, because this build is about to write records in its own
// layout and a downgrade past that point is what the guard must catch.
func ensureFormatVersion(db *badger.DB) error {
	var stored uint32
	err := db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(formatVersionKey))
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return item.Value(func(v []byte) error {
			if len(v) != 4 {
				return fmt.Errorf("malformed value: %d bytes, want 4", len(v))
			}
			stored = binary.BigEndian.Uint32(v)
			return nil
		})
	})
	if err != nil {
		return fmt.Errorf("read %s: %w", formatVersionKey, err)
	}

	if stored > storeFormatVersion {
		return fmt.Errorf("%w: metadata store is at format version %d, this build reads up to %d",
			block.ErrFutureFormat, stored, storeFormatVersion)
	}
	if stored == storeFormatVersion {
		return nil
	}

	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], storeFormatVersion)
	if err := db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(formatVersionKey), buf[:])
	}); err != nil {
		return fmt.Errorf("write %s: %w", formatVersionKey, err)
	}
	return nil
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

// GetUsedBytesForShare returns the logical bytes held by one share's regular
// files. O(1) read of the per-share bucket seeded by initUsedBytesCounter and
// maintained by the transaction delta pipeline.
func (s *BadgerMetadataStore) GetUsedBytesForShare(ctx context.Context, shareName string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	return s.quota.Share(shareName).Bytes, nil
}

// initUsedBytesCounter scans all file entries once at startup to seed the usage
// cache (per share, and per owner identity within a share). It is reconstructed
// from the durable file rows, so a store opened from an existing dump (with no
// separately persisted counters) is always seeded correctly — back-compatible
// by construction.
// A non-nil indexBatch also stages a pl: index entry per file, so a store that
// still needs indexing by payload pays one scan at open rather than two — the
// decode is the expensive part and this is the only place already doing it.
//
// ponytail: one serial decode pass over every file row, so a ten-million-file
// store spends seconds here before the first share opens
// (BenchmarkInitUsedBytesCounter reports the per-file cost). Persisting the
// buckets would remove the pass entirely but has to answer for their
// consistency after a crash, and badger's Stream would parallelize the decode
// at the cost of merging partial sums; do either only once this pass, and not
// the per-share work around it, is what a start is waiting on.
func (s *BadgerMetadataStore) initUsedBytesCounter(indexBatch *badger.WriteBatch) error {
	byIdentity := make(map[basestore.QuotaKey]*metadata.UsageStat)

	addUsage := func(k basestore.QuotaKey, bytes int64) {
		u := byIdentity[k]
		if u == nil {
			u = &metadata.UsageStat{}
			byIdentity[k] = u
		}
		u.Bytes += bytes
		u.Files++
	}

	err := s.db.View(func(txn *badger.Txn) error {
		// An unlinked-but-open inode keeps its row so fstat(2) on a live
		// descriptor still works, but it no longer holds any of the share's
		// bytes. Collect those first — the l: values are four bytes each, so
		// this pass costs a fraction of the file decode below, and only the
		// unlinked ids are retained.
		unlinked := make(map[uuid.UUID]struct{})
		lcOpts := badger.DefaultIteratorOptions
		lcOpts.Prefix = []byte(prefixLinkCount)
		lcOpts.PrefetchValues = true
		lcIt := txn.NewIterator(lcOpts)
		for lcIt.Rewind(); lcIt.Valid(); lcIt.Next() {
			item := lcIt.Item()
			id, idErr := uuid.Parse(string(item.Key()[len(prefixLinkCount):]))
			if idErr != nil {
				continue
			}
			_ = item.Value(func(val []byte) error {
				if c, decErr := decodeUint32(val); decErr == nil && c == 0 {
					unlinked[id] = struct{}{}
				}
				return nil
			})
		}
		lcIt.Close()

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
				_, isUnlinked := unlinked[file.ID]
				if file.Type == metadata.FileTypeRegular && !isUnlinked {
					addUsage(basestore.QuotaKey{Share: file.ShareName, Scope: metadata.QuotaScopeUser, ID: file.UID}, int64(file.Size))
					addUsage(basestore.QuotaKey{Share: file.ShareName, Scope: metadata.QuotaScopeGroup, ID: file.GID}, int64(file.Size))
				}
				return indexFileByPayload(indexBatch, file)
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

	s.quotaMu.Lock()
	s.quota.Seed(byIdentity, nil)
	s.quotaMu.Unlock()
	return nil
}

// GetQuotaUsage returns per-identity usage within one share. O(1) cache read
// under quotaMu. A missing key returns a zero UsageStat.
func (s *BadgerMetadataStore) GetQuotaUsage(shareName string, scope metadata.QuotaScope, id uint32) (metadata.UsageStat, error) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	return s.quota.Get(shareName, scope, id), nil
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
		DBPath:          dbPath,
		Capabilities:    basestore.DefaultCapabilities(),
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
