package fs

import (
	"container/list"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/time/rate"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// backpressureSource is the narrow, read-only window the eviction path
// consults during a remote-cache backpressure stall. It exposes ONLY the
// syncer's drain progress — never the FileBlockStore — preserving invariant
// LSL-08 (eviction must not reach into engine-level metadata). The engine's
// *Syncer satisfies it.
type backpressureSource interface {
	// IsRemoteHealthy reports whether the remote store is reachable, i.e.
	// whether the syncer can drain unsynced chunks and free cache space.
	IsRemoteHealthy() bool
	// UnsyncedBytes is the running total of cache bytes not yet mirrored to
	// remote — the bytes a backpressure stall is waiting to see drained.
	UnsyncedBytes() int64
}

// metricsRecorder is the inline-metrics seam the eviction/backpressure path
// emits to. It aliases the parent-package capability type [local.MetricsRecorder]
// so the engine can wire it via the [local.MetricsAware] capability surface,
// mirroring how the chunk-lifecycle hooks are wired. Keeping it an interface
// keeps this low-level store free of the prometheus dependency; the runtime
// hands the handle down post-construction (shares are built before the metrics
// registry exists). A nil recorder makes every Record* a no-op.
type metricsRecorder = local.MetricsRecorder

// Compile-time assertion: the engine wires metrics via the
// [local.MetricsAware] capability surface (a type assertion on the LocalStore),
// so a signature drift here would otherwise silently disable eviction metrics.
var _ local.MetricsAware = (*FSStore)(nil)

// SetBackpressureSource injects the read-only syncer accessor the eviction
// path consults during a remote-cache stall. Called once during share wiring
// after the syncer is constructed. Passing nil (local-only stores, fixtures)
// leaves ensureSpace on its evictMaxWait fallback. The argument is a narrow
// interface — never the FileBlockStore — per invariant LSL-08.
func (bc *FSStore) SetBackpressureSource(src backpressureSource) {
	bc.bpSource = src
}

// SetMetrics installs the inline metrics recorder for eviction/backpressure
// events. Safe to call after the store is serving (the engine wires it in
// when the runtime hands down its metrics handle); the hot path reads it
// atomically. Passing a nil recorder, or a recorder whose underlying handle
// is nil, makes every Record* a no-op.
func (bc *FSStore) SetMetrics(rec metricsRecorder) {
	bc.metrics.Store(&rec)
}

// recordMetrics returns the installed recorder, or a nil interface if none has
// been wired. The returned value is an interface, so callers must nil-check it
// before invoking a Record* method (a method call on a nil interface panics —
// unlike the nil *metrics.Metrics concrete receiver, which is itself no-op).
func (bc *FSStore) recordMetrics() metricsRecorder {
	if p := bc.metrics.Load(); p != nil {
		return *p
	}
	return nil
}

// BackpressureStats returns the internal backpressure counters: the number
// of times a writer entered a remote-cache stall and the total time writers
// spent stalled. Exposed for tests and a future Prometheus exporter; not
// wired into the REST surface in this iteration.
func (bc *FSStore) BackpressureStats() (engageCount int64, totalStall time.Duration) {
	return bc.bpEngageCount.Load(), time.Duration(bc.bpStallNanos.Load())
}

// retentionConfig holds retention policy settings read/written atomically.
type retentionConfig struct {
	policy block.RetentionPolicy
	ttl    time.Duration
}

// chunkCompleteCallback boxes the OnChunkComplete callback so each
// SetOnChunkComplete swap installs a stable addressable
// *chunkCompleteCallback in atomic.Pointer (a func literal/parameter
// isn't directly addressable). fn may itself be nil — the read path on
// the chunkstore hot path checks fn before invocation.
type chunkCompleteCallback struct {
	fn func(hash block.ContentHash, data []byte, path string)
}

// Errors returned by FSStore.
var (
	ErrStoreClosed    = errors.New("local store: closed")
	ErrDiskFull       = errors.New("local store: disk full after eviction")
	ErrFileNotInStore = errors.New("file not in local store")

	// errLRUEmpty is returned by lruEvictOne when there are no candidates left.
	errLRUEmpty = errors.New("local store: LRU empty, no eviction candidates")
)

// Compile-time interface satisfaction check.
var (
	_ local.LocalStore          = (*FSStore)(nil)
	_ local.ChunkLifecycleHooks = (*FSStore)(nil)
)

// ObjectIDPersister is invoked after a rollup quiesces successfully.
// It receives the payloadID, the BlockRef manifest collected during
// chunking, and the computed ObjectID. Implementations typically
// delegate to a metadata coordinator's PersistFileBlocks. The
// callback is optional; when nil, ObjectID compute still runs but
// the persist step is skipped (local-only / no-engine fixtures).
type ObjectIDPersister func(ctx context.Context, payloadID string, blocks []block.BlockRef, objectID block.ObjectID) error

// FSStore is a two-tier (memory + disk) block store for file data.
//
// NFS WRITE operations (typically 4KB) are buffered in 8MB in-memory blocks.
// When a block fills up or NFS COMMIT is called, the block is flushed atomically
// to a .blk file on disk. This design avoids per-4KB disk I/O and prevents OS
// page cache bloat that caused OOM on earlier versions.
//
// Block metadata (local path, upload state, etc.) is tracked via FileBlockStore
// (BadgerDB) with async batching -- writes are queued in pendingFBs and flushed
// every 200ms by the background goroutine started via Start().
//
// Thread safety: memBlocks uses a dedicated RWMutex (blocksMu) separate from
// the files map (filesMu). Operations on different blocks are fully concurrent
// for reads (RLock). Operations on the same block are serialized via memBlock.mu.
//
// Lock ordering: blocksMu -> mb.mu (never the reverse).
// In flushBlock, the map entry is deleted while holding mb.mu to prevent a race
// where a concurrent writer gets a stale memBlock with nil data.
type FSStore struct {
	baseDir   string
	maxDisk   int64
	maxMemory int64
	// the local store is one of the engine-
	// internal callers that still uses the wider EngineFileBlockStore
	// surface (GetFileBlock, ListFileBlocks) on top of the narrowed
	// FileBlockStore. routes reads through FileAttr.Blocks
	// and lets us drop the wider interface entirely.
	blockStore block.EngineFileBlockStore

	// blocksMu guards the memBlocks and fileBlocks maps. Uses RWMutex for
	// concurrent reads (the common case: checking if a block is already buffered).
	// RWMutex outperforms sync.Map for write-heavy workloads with high key
	// churn (random writes that create/flush/recreate blocks frequently).
	blocksMu   sync.RWMutex
	memBlocks  map[blockKey]*memBlock
	fileBlocks map[string]map[uint64]*memBlock // payloadID -> blockIdx -> mb

	// filesMu guards the files map separately from block operations.
	filesMu sync.RWMutex
	files   map[string]*fileInfo

	closedFlag atomic.Bool

	memUsed  atomic.Int64
	diskUsed atomic.Int64

	// flushFsyncCount counts how many times flushBlock has invoked fsync.
	// Test-only observable surfaced via FlushFsyncCountForTest. Incremented
	// only on the COMMIT-driven path (withFsync=true); pressure and
	// block-fill paths flush without fsync and do not bump this counter.
	flushFsyncCount atomic.Int64

	// pendingFBs queues FileBlock metadata updates for async persistence.
	// Keyed by blockID (string) -> *block.FileBlock.
	// Drained every 200ms by SyncFileBlocks, and on Close/Flush.
	pendingFBs sync.Map

	// diskIndex is the authoritative in-process cache of FileBlock metadata
	// for blocks currently stored on the local filesystem. It is populated
	// whenever a block lands on disk (flushBlock, tryDirectDiskWrite eager-
	// create, WriteFromRemote, Recover) and pruned on delete/evict. The
	// write hot path and eviction consult diskIndex INSTEAD of the
	// FileBlockStore, decoupling those paths from the underlying metadata
	// backend.
	//
	// Entries survive pendingFBs drain so subsequent hot-path operations
	// (e.g., a second pwrite into the same block after BadgerDB persistence)
	// can still observe the block's existing State / BlockStoreKey without
	// a FileBlockStore round-trip.
	//
	// Keyed by blockID (string) -> *block.FileBlock. The stored pointer
	// is SHARED with pendingFBs so queued updates mutate a single instance.
	diskIndex sync.Map

	// fdPool pools open file descriptors for .blk files to avoid
	// open+close syscalls on every 4KB random write in tryDirectDiskWrite.
	fdPool *fdPool

	// readFDPool pools open file descriptors (O_RDONLY) for .blk files
	// to avoid open+close syscalls on every 4KB random read in readFromDisk.
	readFDPool *fdPool

	// evictionEnabled controls whether ensureSpace can evict blocks.
	// When false, ensureSpace returns ErrDiskFull if over limit instead of
	// evicting remote blocks. Used by local-only mode where there is no
	// remote store to re-fetch evicted blocks from.
	evictionEnabled atomic.Bool

	// retention holds the retention policy and TTL, accessed atomically
	// to avoid data races between SetRetentionPolicy and concurrent eviction.
	retention unsafe.Pointer // *retentionConfig

	// accessTracker maintains per-file last-access times for eviction ordering.
	// Updated on read/write paths via Touch(), queried during eviction.
	accessTracker *accessTracker

	// done signals the Start goroutine to exit. Closed exactly once by Close
	// (guarded by closeOnce) so the goroutine can terminate independently of
	// the ctx passed to Start. wg joins the Start goroutine so Close() returns
	// only after the goroutine has exited — no leaked goroutines across
	// repeated Start/Close cycles.
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	// --- Append-log path. ---
	//
	// Append is mandatory on the local tier — the flag-gated opt-out was
	// deleted alongside the legacy path-keyed writer. All AppendWrite +
	// rollup machinery runs unconditionally.
	maxLogBytes     int64
	stabilizationMS int
	rollupWorkers   int

	// pressureMaxWait caps how long AppendWrite blocks in the pressure
	// loop before returning ErrPressureTimeout. Defense-in-depth against
	// a wedged rollup (#670): NFS COMMIT and SMB Flush callers commonly
	// arrive with no usable deadline, so without an upper bound here a
	// stuck rollup translates directly into D-state on the client.
	//
	// Internal encoding: 0 means "deadline disabled" (no timer armed in
	// AppendWrite's pressure loop). The default 30s is installed by
	// newFSStore before any option override runs; the only way
	// this field is 0 post-construction is via an explicit negative
	// FSStoreOptions.PressureMaxWait (test-only). Direct struct-literal
	// construction of FSStore is not supported.
	pressureMaxWait time.Duration

	// evictMaxWait caps how long ensureSpace back-pressures (waiting for
	// new evictable chunks to land) before returning ErrDiskFull when the
	// LRU has no evictable candidate. Defaults to 30s (installed by
	// newFSStore). Unexported with no public option: only same-package
	// tests shrink it to avoid a 30s wall-clock wait when asserting the
	// unsynced-only-LRU back-pressure path.
	evictMaxWait time.Duration

	// backpressureMaxWait caps how long ensureSpace stalls a writer while
	// the LRU has no evictable candidate (every cached chunk is still
	// unsynced) but the remote is HEALTHY — i.e. the syncer can still drain
	// and free cache space. Distinct from evictMaxWait: this is the
	// remote-cache backpressure window. When the remote is unhealthy (the
	// syncer cannot drain), ensureSpace returns ErrDiskFull immediately
	// rather than waiting out this window. Defaults to 60s (installed by
	// newFSStore); same-package tests shrink it.
	backpressureMaxWait time.Duration

	// bpSource is the read-only seam into the syncer's drain progress that
	// ensureSpace consults during the backpressure window: whether the
	// remote is healthy (can drain) and how many cache bytes are still
	// unsynced (whether a stall can make progress). Injected post-
	// construction via SetBackpressureSource — nil on local-only stores and
	// in fixtures, in which case ensureSpace falls back to evictMaxWait
	// semantics. NEVER hand the FileBlockStore here: this is a narrow,
	// non-FileBlockStore interface, preserving invariant LSL-08.
	bpSource backpressureSource

	// Backpressure observability counters (internal; not yet exposed via
	// REST — a future Prometheus pass reads them directly). bpEngageCount
	// increments once each time a writer enters the remote-cache stall;
	// bpStallNanos accumulates the total time writers spent stalled. Both
	// are atomic so concurrent writers can update them lock-free.
	bpEngageCount atomic.Int64
	bpStallNanos  atomic.Int64

	// metrics is the inline Prometheus recorder for eviction/backpressure
	// events. Held behind an atomic.Pointer because the share-wiring path
	// constructs this store BEFORE the metrics registry exists, so the
	// runtime swaps the handle in post-construction via SetMetrics while the
	// hot path (lruEvictOne, ensureSpace) reads it concurrently. The pointer
	// always holds a stable addressable metricsRecorder; the inner value may
	// be a nil *metrics.Metrics, whose Record* methods are themselves no-ops.
	metrics atomic.Pointer[metricsRecorder]

	// bpLogLimiter rate-limits the engage/release Info logs so a sustained
	// fast-writer / slow-uploader workload does not flood the log.
	bpLogLimiter *rate.Limiter

	// logBytesTotal is the current total bytes of un-rolled-up log content
	// across every payloadID in this FSStore. Incremented by AppendWrite
	// (framed-record size) and decremented by the rollup.
	// AppendWrite blocks on pressureCh when logBytesTotal > maxLogBytes
	logBytesTotal atomic.Int64

	// pressureCh is a buffered channel (size 1) that the rollup pulses after
	// freeing log budget. AppendWrite selects on it inside the pressure loop
	// along with ctx.Done() and bc.done.
	pressureCh chan struct{}

	// logShards stripes all per-payload append-log state across
	// numLogShards independent RWMutex-guarded maps, keyed by payloadID
	// hash (shardFor). Before C2 a single RWMutex (logsMu) guarded every
	// map; getOrCreateLog's write-lock on the create path then serialized
	// every new-payload acquisition across the WHOLE store, which the #680
	// re-profile measured as ~60% of mutex-wait under a concurrent
	// create/delete storm (REVIEW.md §5.1 C2). Sharding confines that
	// write-lock to 1/numLogShards of the keyspace.
	//
	// Lock order is unchanged by sharding: the per-file rollup mutex (rmu)
	// and append mutex (mu) are still acquired BEFORE a shard lock (the
	// FIX-2 "mu before the map lock" discipline), and a shard lock is never
	// held while acquiring a different shard's lock — cross-payload sweeps
	// lock exactly one shard at a time. Each payloadID maps to exactly one
	// shard, so all of a payload's state (fd, mutexes, tree, index,
	// tombstone, truncation) lives under that single shard lock, preserving
	// the #668 "tree and logIndex created under the same lock" invariant.
	logShards []*logShard

	// --- -06 rollup pool. ---
	//
	// StartRollup launches bc.rollupWorkers goroutines that consume
	// payloadIDs from bc.rollupCh (AppendWrite non-blocking send) and also
	// scan bc.dirtyIntervals on a stabilization-tuned ticker.
	rollupStore   metadata.RollupStore
	rollupCh      chan string
	rollupStarted atomic.Bool
	rollupWg      sync.WaitGroup

	// stopRollup signals the rollup worker pool to stop accepting NEW
	// rollups and exit, WITHOUT marking the whole store closed. Closed
	// exactly once by GracefulStopRollup (guarded by stopRollupOnce) so
	// the workers join before a fresh-context drain runs. Distinct from
	// `done` (full-Close signal): graceful shutdown wants to halt the
	// cancellable-ctx workers and then DRAIN the remaining dirty payloads
	// on a separate, non-cancelled context (#1245 Bug C) before the store
	// is actually closed.
	stopRollup     chan struct{}
	stopRollupOnce sync.Once

	// rollupDrainGraceDur bounds how long the graceful shutdown drain
	// (GracefulStopRollup, invoked from Close) spends flushing in-flight /
	// remaining rollups on its fresh, non-cancelled context before giving up
	// and letting the remainder resume on the next boot. Default 30s (set by
	// newFSStore); operator-tunable via FSStoreOptions.RollupDrainGrace. A
	// non-positive value defers to the 30s default inside GracefulStopRollup.
	rollupDrainGraceDur time.Duration

	// syncedHashStore persists per-CAS-hash local→remote sync state.
	// Consumed by ListUnsynced to filter the Walk-collected hash set
	// down to the still-unmirrored subset. Nil-valued on local-only
	// stores (no remote configured); in that case ListUnsynced yields
	// nothing.
	syncedHashStore metadata.SyncedHashStore

	// objectIDPersister is invoked after a rollup quiesces successfully.
	// rollupFile passes the chunker's accumulated BlockRef manifest and
	// the BLAKE3 Merkle-root ObjectID derived from it. Nil-valued on
	// local-only / no-engine fixtures; in that case ObjectID compute
	// still runs but the persist step is skipped harmlessly.
	//
	// Read under persisterMu so SetObjectIDPersister can install a
	// coordinator-backed closure after the rollup workers have already
	// been launched (engine.New runs after StartRollup in the share-
	// service wiring path).
	persisterMu       sync.RWMutex
	objectIDPersister ObjectIDPersister

	// ---: in-process LRU for CAS chunks. ---
	//
	// Eviction is driven entirely from on-disk presence under
	// blocks/{hh}/{hh}/{hex}, indexed by ContentHash in lruIndex/lruList.
	// Eviction = unlink the file directly; no FileBlockStore lookup happens
	// on the write hot path.
	//
	// Population
	// - StoreChunk(...) -> lruTouch on rename success.
	// - ReadChunk(...) -> lruTouch on read success (re-promote).
	//   - seedLRUFromDisk() -> alphabetical scan at New() so warm-started
	//                          stores have a deterministic LRU position.
	//
	// Mutations (Touch / EvictOne) are serialized by lruMu. Disk unlinks
	// happen under lruMu; concurrent ReadChunk that races an evict surfaces
	// as ENOENT and falls through to the engine refetch path (T-11-B-08).
	lruMu    sync.Mutex
	lruIndex map[block.ContentHash]*list.Element // *lruEntry
	lruList  *list.List                          // most-recent at front

	// ---: hash dedup LRU + chunk-complete hook. ---
	//
	// dedupLRU is the per-FSStore hash dedup LRU (Opt 1).
	// Consulted by rollup.go between FastCDC.Next() and
	// StoreChunk to skip a metadata round-trip on hot hashes. RAM-only
	// instantiated unconditionally in newFSStore.
	dedupLRU *dedupLRU

	// onChunkComplete fires once per successful chunkstore.StoreChunk
	// (immediately after that path's lruTouch). The ReadChunk path also
	// touches the LRU but does not fire the callback — only the rollup
	// pool's StoreChunk completion is reported. Install via
	// FSStoreOptions at construction or post-hoc via SetOnChunkComplete;
	// the engine's Cache materializes in BlockStore.Start, AFTER
	// cfg.Local is constructed, so the setter path is the production
	// wire-in site.
	//
	// Stored via atomic.Pointer so SetOnChunkComplete can swap the
	// callback safely while rollup workers read it on the hot path in
	// chunkstore.StoreChunk. The pointer always holds a stable
	// addressable *chunkCompleteCallback so the read path never observes
	// a nil pointer — the inner fn may itself be nil and is checked
	// there.
	onChunkComplete atomic.Pointer[chunkCompleteCallback]

	// compactionThresholdBytes is the minimum number of pre-fence bytes
	// that must accumulate in a per-payload log before the next rollup
	// pass triggers physical compaction (rewrites the log file dropping
	// records below idx.compactionFence). Default = maxLogBytes /
	// defaultCompactionDivisor when FSStoreOptions.CompactionThresholdBytes
	// is zero. Set to a negative value to disable compaction entirely
	// (the log file then grows until DeleteAppendLog wipes the payload
	// matching pre-#579 behavior).
	//
	// Trade-off: a smaller threshold caps disk growth tighter at the
	// cost of more I/O churn (per-pass we copy `survivors` bytes); a
	// larger threshold lets the log grow further between passes.
	compactionThresholdBytes int64

	// orphanLogMinAgeSeconds gates the append-log orphan sweep during
	// recovery. A log is considered orphan only when
	// (a) its rollup_offset in metadata is 0, (b) no FileBlock exists for
	// block-0 of the payloadID, AND (c) the log file's mtime is older
	// than this threshold. The mtime gate guarantees freshly-created logs
	// with no rolled-up metadata yet are never swept at boot.
	//
	// Default 3600 (1h) is set by NewWithOptions when the option is left
	// at zero. Configurable via
	// `blockstore.local.fs.orphan_log_min_age_seconds` (wiring).
	orphanLogMinAgeSeconds int
}

// newFSStore is the shared inner constructor used by NewWithOptions and
// NewFSStoreForMigration. It runs the legacy-layout sentinel gate when
// skipSentinelCheck is false; the migration path passes true to open a
// share that still holds the legacy `.blk` layout the migration is
// converting away from.
//
// Four-state sentinel matrix:
//
//   - sentinel PRESENT, no .blk files       → success (post-migration steady state)
//   - sentinel PRESENT, .blk files PRESENT  → success (sentinel is ground truth)
//   - sentinel MISSING, no .blk files       → success (fresh install)
//   - sentinel MISSING, .blk files PRESENT  → block.ErrLegacyLayoutDetected
func newFSStore(baseDir string, maxDisk, maxMemory int64, fileBlockStore block.EngineFileBlockStore, opts FSStoreOptions, skipSentinelCheck bool) (*FSStore, error) {
	if !skipSentinelCheck {
		if err := checkLegacyLayoutSentinel(baseDir); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("local store: create base dir: %w", err)
	}

	if maxMemory <= 0 {
		maxMemory = 256 * 1024 * 1024 // 256MB default
	}

	defaultRetention := &retentionConfig{policy: block.RetentionLRU}
	bc := &FSStore{
		baseDir:       baseDir,
		maxDisk:       maxDisk,
		maxMemory:     maxMemory,
		blockStore:    fileBlockStore,
		memBlocks:     make(map[blockKey]*memBlock),
		fileBlocks:    make(map[string]map[uint64]*memBlock),
		files:         make(map[string]*fileInfo),
		fdPool:        newFDPool(defaultFDPoolSize),
		readFDPool:    newFDPool(defaultFDPoolSize),
		accessTracker: newAccessTracker(),
		retention:     unsafe.Pointer(defaultRetention),
		done:          make(chan struct{}),
		stopRollup:    make(chan struct{}),
	}
	bc.evictionEnabled.Store(true)

	// in-process LRU for CAS chunks (see field comments).
	bc.lruIndex = make(map[block.ContentHash]*list.Element)
	bc.lruList = list.New()

	// append-log plumbing — maps + pressure channel are always
	// initialized; the opt-out flag was deleted with the legacy
	// path-keyed writer.
	bc.pressureCh = make(chan struct{}, 1)
	bc.logShards = make([]*logShard, numLogShards)
	for i := range bc.logShards {
		bc.logShards[i] = newLogShard()
	}
	bc.maxLogBytes = 1 << 30                  // 1 GiB default
	bc.stabilizationMS = 250                  // default
	bc.rollupWorkers = 2                      // default
	bc.pressureMaxWait = 30 * time.Second     // default — #670 defense-in-depth
	bc.rollupDrainGraceDur = 30 * time.Second // default — #1245 shutdown drain window
	bc.evictMaxWait = 30 * time.Second        // default ensureSpace back-pressure cap
	bc.backpressureMaxWait = 60 * time.Second // default remote-cache backpressure window
	// Rate-limit backpressure engage/release logs: at most one line every
	// 5s plus a small burst, so a sustained stall does not flood the log
	// while still surfacing the first engage and the eventual release.
	bc.bpLogLimiter = rate.NewLimiter(rate.Every(5*time.Second), 2)
	// rollupCh buffered so AppendWrite's non-blocking send rarely drops
	// on drop, the ticker arm in chunkRollupWorker picks up the payload
	// on the next scan.
	bc.rollupCh = make(chan string, bc.rollupWorkers*4)

	// Seed the in-process LRU from any chunks already present on
	// disk under blocks/{hh}/{hh}/{hex}. Cold-start order is alphabetical
	// for determinism; subsequent reads/writes promote chunks to the front.
	bc.seedLRUFromDisk()

	applyFSStoreOptions(bc, opts)
	return bc, nil
}

// applyFSStoreOptions overlays caller-supplied option values on top of
// the construction-time defaults. Zero values keep the default; positive values
// override; negative values (where supported) carry explicit
// "disable" semantics — see the individual field godocs on
// FSStoreOptions.
func applyFSStoreOptions(bc *FSStore, opts FSStoreOptions) {
	if opts.MaxLogBytes > 0 {
		bc.maxLogBytes = opts.MaxLogBytes
	}
	if opts.RollupWorkers > 0 {
		bc.rollupWorkers = opts.RollupWorkers
		// Re-size rollupCh to match the caller-specified pool size so
		// the non-blocking send in AppendWrite has proportional
		// headroom.
		bc.rollupCh = make(chan string, bc.rollupWorkers*4)
	}
	if opts.StabilizationMS > 0 {
		bc.stabilizationMS = opts.StabilizationMS
	}
	switch {
	case opts.PressureMaxWait > 0:
		bc.pressureMaxWait = opts.PressureMaxWait
	case opts.PressureMaxWait < 0:
		// Negative explicitly disables the deadline (block forever).
		// Required for tests that drive the pressure loop directly
		// without a rollup worker; not recommended in production.
		bc.pressureMaxWait = 0
	}
	if opts.BackpressureMaxWait > 0 {
		bc.backpressureMaxWait = opts.BackpressureMaxWait
	}
	if opts.RollupDrainGrace > 0 {
		bc.rollupDrainGraceDur = opts.RollupDrainGrace
	}
	bc.rollupStore = opts.RollupStore
	bc.syncedHashStore = opts.SyncedHashStore
	bc.objectIDPersister = opts.ObjectIDPersister
	if opts.OrphanLogMinAgeSeconds > 0 {
		bc.orphanLogMinAgeSeconds = opts.OrphanLogMinAgeSeconds
	} else {
		bc.orphanLogMinAgeSeconds = 3600
	}

	// Compaction threshold (#579). Zero → derive from maxLogBytes so a
	// smaller log budget compacts proportionally more aggressively; a
	// negative value disables compaction entirely (pre-#579 behavior
	// useful for tests pinned to the legacy growth pattern).
	switch {
	case opts.CompactionThresholdBytes > 0:
		bc.compactionThresholdBytes = opts.CompactionThresholdBytes
	case opts.CompactionThresholdBytes < 0:
		bc.compactionThresholdBytes = -1
	default:
		bc.compactionThresholdBytes = bc.maxLogBytes / defaultCompactionDivisor
		if bc.compactionThresholdBytes <= 0 {
			// maxLogBytes < defaultCompactionDivisor — clamp so a tiny
			// budget still gets a sensible threshold floor.
			bc.compactionThresholdBytes = int64(logHeaderSize)
		}
	}

	// per-FSStore hash dedup LRU + chunk-complete hook. Default-on-zero
	// idiom matches existing FSStoreOptions tunables.
	size := opts.DedupLRUSize
	if size <= 0 {
		size = 4096
	}
	bc.dedupLRU = newDedupLRU(size)
	// Install a non-nil holder so the hot-path Load never observes nil;
	// opts.OnChunkComplete may itself be nil, which is checked at the
	// firing site.
	bc.onChunkComplete.Store(&chunkCompleteCallback{fn: opts.OnChunkComplete})
}

// sentinelFileName is the per-share boot-guard marker written by
// `dfs migrate-to-cas` at the successful completion of a share migration.
// Kept in sync with pkg/block/migrate.SentinelFileName.
const sentinelFileName = ".cas-migrated-v1"

// legacyLayoutWalkDepthCap bounds the depth-limited `.blk` probe so a
// freshly-provisioned share with deeply nested non-legacy content does
// not pay an unbounded WalkDir at every boot. Legacy `.blk` files live
// at <baseDir>/blocks/<shard>/<payloadID>/<idx>.blk (depth 4 under
// baseDir after the v0.16 follow-up that dropped the redundant outer
// `blocks/` parent — pre-follow-up was depth 3 under <baseDir>/blocks).
// Any `.blk` past depth 4 is treated as non-legacy noise and skipped.
const legacyLayoutWalkDepthCap = 4

// checkLegacyLayoutSentinel implements. Returns
// block.ErrLegacyLayoutDetected (wrapped with the share path) when
// the `.cas-migrated-v1` sentinel is absent from baseDir AND at least
// one `.blk` file is detected by a depth-capped WalkDir under baseDir.
// Returns nil for the other three states (sentinel present, or sentinel
// absent + no legacy data).
//
// Boot-path performance: post-migration the first stat short-circuits
// no walk. Pre-migration on legacy stores, the walk terminates at the
// first `.blk` via fs.SkipAll. Fresh installs with no baseDir yet hit
// the iofs.ErrNotExist branch and fall through to construction.
func checkLegacyLayoutSentinel(baseDir string) error {
	sentinelPath := filepath.Join(baseDir, sentinelFileName)
	if _, err := os.Stat(sentinelPath); err == nil {
		// Sentinel present — trusts it as ground truth.
		return nil
	} else if !errors.Is(err, iofs.ErrNotExist) {
		return fmt.Errorf("local store: stat sentinel %q: %w", sentinelPath, err)
	}

	// Sentinel missing — probe for `.blk` files. A baseDir that does
	// not yet exist is a fresh install with no legacy data.
	hasBlk := false
	walkErr := filepath.WalkDir(baseDir, func(path string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Depth from baseDir: 0 = baseDir itself, 1 = direct child, …
		rel := strings.TrimPrefix(path, baseDir)
		rel = strings.TrimPrefix(rel, string(os.PathSeparator))
		var depth int
		if rel != "" {
			depth = strings.Count(rel, string(os.PathSeparator)) + 1
		}
		if d.IsDir() {
			if depth > legacyLayoutWalkDepthCap {
				return iofs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".blk") {
			hasBlk = true
			return iofs.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		if errors.Is(walkErr, iofs.ErrNotExist) {
			// baseDir does not exist yet — fresh install with no legacy
			// data; MkdirAll downstream will create it.
			return nil
		}
		return fmt.Errorf("local store: probe legacy layout %q: %w", baseDir, walkErr)
	}
	if !hasBlk {
		return nil
	}
	return fmt.Errorf("share %s: %w", baseDir, block.ErrLegacyLayoutDetected)
}

// lruEntry is a single LRU-tracked CAS chunk on disk.
type lruEntry struct {
	hash block.ContentHash
	size int64
	path string // absolute path under <baseDir>/blocks/<hh>/<hh>/<hex>
}

// lruTouch promotes (or inserts) a chunk to the most-recent end of the LRU.
// Called from StoreChunk on rename success and from ReadChunk on cache hit.
// Safe to call from concurrent goroutines.
func (bc *FSStore) lruTouch(h block.ContentHash, size int64, path string) {
	bc.lruMu.Lock()
	defer bc.lruMu.Unlock()
	if el, ok := bc.lruIndex[h]; ok {
		bc.lruList.MoveToFront(el)
		// Update size/path in case the chunk was re-stored (idempotent CAS).
		entry := el.Value.(*lruEntry)
		entry.size = size
		entry.path = path
		return
	}
	el := bc.lruList.PushFront(&lruEntry{hash: h, size: size, path: path})
	bc.lruIndex[h] = el
}

// lruEvictOne removes the least-recently-used EVICTABLE chunk from the
// LRU and unlinks its on-disk file. Returns the freed byte count, or 0 +
// sentinel when there are no evictable candidates left. Concurrent
// ReadChunk that races an evict surfaces as block.ErrChunkNotFound
// (T-11-B-08, accept/refetch posture).
//
// A chunk is NOT evictable until it has been mirrored to remote: evicting
// an unsynced chunk before its first upload silently destroys the only
// copy. When a SyncedHashStore is wired, each candidate is consulted via
// IsSynced; an unsynced candidate is moved to the FRONT of the LRU (the
// most-recent end, away from the eviction tail) so the scan advances to
// the next-oldest candidate instead of re-popping the same chunk. To
// avoid spinning forever when every candidate is unsynced, the scan visits
// at most a one-shot snapshot of the LRU length before giving up with
// errLRUEmpty (ensureSpace then waits/back-pressures with ErrDiskFull).
// When no SyncedHashStore is wired (local-only, no remote), every chunk is
// evictable and the IsSynced step is skipped.
//
// IsSynced is called OUTSIDE lruMu: the SyncedHashStore has its own
// internal mutex, so holding lruMu across the call would invert lock
// ordering against the StoreChunk/touch path, and IsSynced may be slow on
// the badger/postgres backends.
//
// Race-free design (optimistic peek-recheck): the candidate is NEVER
// removed from lruIndex during the unlocked IsSynced call. Earlier code
// popped the tail (list+index) before IsSynced and re-pushed afterwards;
// during that unlocked window the entry was ABSENT from lruIndex, so a
// concurrent ReadChunk/StoreChunk lruTouch for the same hash would insert
// a SECOND list element — leaving lruIndex pointing at only one of two
// duplicates (ghost entries + wrong disk accounting). Instead this loop:
//  1. PEEKS the tail under lruMu (reads hash+size+path, leaves it in
//     list/index), releases lruMu.
//  2. Calls IsSynced unlocked.
//  3. Re-acquires lruMu and VERIFIES the tail is still the same entry
//     (same element, same hash, still indexed). A concurrent lruTouch may
//     have moved it to the front in the meantime; if so it is no longer a
//     victim, so we drop it and retry the peek loop. If still the tail and
//     synced, remove+unlink under the recheck. If still the tail and
//     unsynced, move it to the front and continue scanning.
//
// Because the entry is never absent from lruIndex during the unlocked
// IsSynced, a concurrent lruTouch always finds it and moves it normally —
// no ghost entries can form.
func (bc *FSStore) lruEvictOne(ctx context.Context) (int64, error) {
	// Snapshot the candidate budget under lruMu: at most this many
	// unsynced/moved peeks before declaring "no evictable candidates".
	bc.lruMu.Lock()
	budget := bc.lruList.Len()
	bc.lruMu.Unlock()

	for attempts := 0; attempts < budget; attempts++ {
		// 1. PEEK the tail without removing it from list/index.
		bc.lruMu.Lock()
		el := bc.lruList.Back()
		if el == nil {
			bc.lruMu.Unlock()
			return 0, errLRUEmpty
		}
		entry := el.Value.(*lruEntry)
		hash := entry.hash
		bc.lruMu.Unlock()

		// 2. Consult sync state OUTSIDE lruMu. Skip entirely for
		//    local-only stores (no SyncedHashStore wired) — every chunk
		//    is evictable there.
		evictable := true
		if bc.syncedHashStore != nil {
			synced, err := bc.syncedHashStore.IsSynced(ctx, hash)
			if err != nil {
				// Treat lookup failures as unsynced: refuse to evict on
				// uncertainty rather than risk destroying the only copy.
				logger.Warn("lruEvictOne: IsSynced lookup failed, treating chunk as unsynced",
					"hash", hash.String(), "error", err)
				evictable = false
			} else {
				evictable = synced
			}
		}

		// 3. Re-acquire lruMu and recheck the tail. A concurrent
		//    lruTouch/StoreChunk may have moved this entry off the tail
		//    during the unlocked IsSynced call.
		bc.lruMu.Lock()
		cur := bc.lruList.Back()
		idxEl, stillIndexed := bc.lruIndex[hash]
		if cur != el || !stillIndexed || idxEl != el {
			// The tail changed (entry was touched/moved/removed): it is no
			// longer the eviction victim. Drop it and retry the peek loop.
			bc.lruMu.Unlock()
			continue
		}

		if !evictable {
			// Still the tail but unsynced: move it to the FRONT (away from
			// the eviction tail) so the scan advances to the next-oldest
			// candidate instead of re-popping this same chunk.
			bc.lruList.MoveToFront(el)
			bc.lruMu.Unlock()
			continue
		}

		// Still the tail AND synced: remove from list+index under the
		// recheck, then unlink the file.
		path := entry.path
		size := entry.size
		bc.lruList.Remove(el)
		delete(bc.lruIndex, hash)
		bc.lruMu.Unlock()

		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			// File system error: re-insert to avoid losing the bookkeeping
			// (the chunk is still on disk, it just couldn't be unlinked).
			bc.lruMu.Lock()
			bc.lruIndex[hash] = bc.lruList.PushBack(entry)
			bc.lruMu.Unlock()
			return 0, fmt.Errorf("evict %s: %w", path, err)
		}
		if rec := bc.recordMetrics(); rec != nil {
			rec.RecordEviction(size)
		}
		return size, nil
	}

	// Every candidate within the snapshot budget was unsynced or moved off
	// the tail (or the LRU was empty to begin with): no evictable chunk.
	return 0, errLRUEmpty
}

// seedLRUFromDisk walks <baseDir>/blocks/ at startup and registers every
// chunk file in the LRU. Order is deterministic (alphabetical hash hex)
// so repeated startups produce the same cold-start eviction order.
//
// Best-effort: any per-file error is silently skipped (the next StoreChunk
// or ReadChunk will register the chunk on demand).
func (bc *FSStore) seedLRUFromDisk() {
	blocksDir := filepath.Join(bc.baseDir, "blocks")
	if _, err := os.Stat(blocksDir); err != nil {
		return
	}
	type seed struct {
		hash block.ContentHash
		size int64
		path string
	}
	var seeds []seed
	_ = filepath.WalkDir(blocksDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		// Path layout: <baseDir>/blocks/<hh>/<hh>/<hex>.
		name := d.Name()
		if len(name) != block.HashSize*2 {
			return nil
		}
		raw, err := hex.DecodeString(name)
		if err != nil || len(raw) != block.HashSize {
			return nil
		}
		var h block.ContentHash
		copy(h[:], raw)
		info, err := d.Info()
		if err != nil {
			return nil
		}
		seeds = append(seeds, seed{hash: h, size: info.Size(), path: path})
		return nil
	})
	// Sort alphabetically by hash hex for deterministic cold-start order.
	// Files visited by WalkDir are already lexicographically ordered within
	// directories, but cross-directory order depends on filesystem
	// traversal — sort explicitly to be safe.
	sort.Slice(seeds, func(i, j int) bool {
		return seeds[i].hash.String() < seeds[j].hash.String()
	})
	for _, s := range seeds {
		bc.lruTouch(s.hash, s.size, s.path)
	}
}

// FSStoreOptions configures the append-log path. Append is mandatory in
// — the UseAppendLog opt-out flag was deleted with the legacy
// path-keyed writer. MaxLogBytes, RollupWorkers, and StabilizationMS
// default via NewWithOptions when left zero here.
type FSStoreOptions struct {
	MaxLogBytes     int64
	RollupWorkers   int
	StabilizationMS int
	// RollupStore persists per-file rollup_offset. Required
	// when StartRollup will be called. Nil is accepted when the caller
	// will not start the rollup pool.
	RollupStore metadata.RollupStore
	// SyncedHashStore persists per-CAS-hash local→remote sync state.
	// Required when a remote store is configured (the engine's Syncer
	// consumes it via ListUnsynced + MarkSynced). Nil is accepted for
	// local-only stores; in that case ListUnsynced yields nothing.
	SyncedHashStore metadata.SyncedHashStore
	// ObjectIDPersister is the rollup-completion hook that receives the
	// BlockRef manifest + computed ObjectID after SetRollupOffset
	// succeeds. Wire this to the engine coordinator's PersistFileBlocks
	// so local-only and remote-backed shares both materialize ObjectIDs
	// at rollup time. Nil is accepted: ObjectID is still computed, but
	// the persist call is skipped (local-only fixtures / no-engine
	// fixtures).
	ObjectIDPersister ObjectIDPersister
	// OrphanLogMinAgeSeconds is the minimum age (seconds) a log file must
	// have before recovery will classify it as orphan and sweep it.
	// Defaults to 3600 (1h) when zero. See FSStore.orphanLogMinAgeSeconds
	// for the rationale.
	OrphanLogMinAgeSeconds int

	// OnChunkComplete is invoked once per successful chunkstore.lruTouch
	// (post-disk-store), with the chunk's content hash, bytes, and on-disk
	// path. Wire to engine.Cache.Put to populate the read cache at write
	// time. Nil is accepted: chunkstore behaves identically to the
	// pre-callback path if absent. Callback MUST be non-blocking on
	// hot paths.
	OnChunkComplete func(hash block.ContentHash, data []byte, path string)

	// DedupLRUSize is the slot count for the in-memory hash dedup LRU.
	// Default 4096 when zero. Per-FSStore scope, RAM-only. Surfaced
	// via pkg/config as blockstore.local.dedup_lru_size.
	DedupLRUSize int

	// PressureMaxWait bounds how long AppendWrite blocks in its pressure
	// loop (logBytesTotal > maxLogBytes) before returning
	// ErrPressureTimeout. Zero defers to the 30s default. A negative
	// value disables the deadline (block indefinitely; not recommended
	// in production).
	//
	// Defense-in-depth against a wedged rollup pool (#670). NFS COMMIT
	// and SMB Flush adapters frequently arrive with no usable deadline,
	// so without this bound a stuck rollup wedges the client process in
	// D-state. Pick an upper-bound value clearly larger than any
	// legitimate rollup latency under load — this is NOT an SLA.
	PressureMaxWait time.Duration

	// BackpressureMaxWait bounds how long ensureSpace stalls a writer while
	// every cached chunk is unsynced but the remote is healthy (the syncer
	// can still drain). Zero defers to the 60s default. Distinct from the
	// internal evict wait. See FSStore.backpressureMaxWait.
	BackpressureMaxWait time.Duration

	// RollupDrainGrace bounds how long the graceful shutdown drain
	// (GracefulStopRollup, invoked from Close) spends flushing in-flight /
	// remaining rollups on a fresh, non-cancelled context before letting the
	// remainder resume on the next boot (#1245 Bug C). Zero defers to the 30s
	// default. The drain is best-effort: exceeding it is non-fatal because the
	// append log is durable and rollups are idempotent on restart.
	RollupDrainGrace time.Duration

	// CompactionThresholdBytes controls when physical log compaction
	// runs (#579). After a rollup pass advances idx.compactionFence
	// the rewrite is invoked if the fence sits more than this many
	// bytes above logHeaderSize. Zero defers to the default
	// (maxLogBytes / defaultCompactionDivisor). A negative value
	// disables compaction entirely (pre-#579 behavior — the log grows
	// until DeleteAppendLog).
	CompactionThresholdBytes int64
}

// NewWithOptions constructs an FSStore.
//
// Parameters:
//   - baseDir: directory for .blk block files, created if absent.
//   - maxDisk: maximum total size of on-disk .blk files in bytes. 0 = unlimited.
//   - maxMemory: memory budget for dirty write buffers in bytes. 0 defaults to 256MB.
//   - fileBlockStore: persistent store for FileBlock metadata
//     (local path, upload state, etc.).
//   - opts: tunables for append-log sizing, rollup pool, dedup LRU,
//     chunk-complete hook, etc. Zero-valued fields fall back to the
//     defaults documented on FSStoreOptions.
//
// Runs the legacy-layout sentinel gate; returns
// block.ErrLegacyLayoutDetected when the share holds the pre-CAS
// `.blk` layout without a `.cas-migrated-v1` marker.
func NewWithOptions(baseDir string, maxDisk, maxMemory int64, fileBlockStore block.EngineFileBlockStore, opts FSStoreOptions) (*FSStore, error) {
	return newFSStore(baseDir, maxDisk, maxMemory, fileBlockStore, opts, false)
}

// NewFSStoreForMigration constructs an FSStore that skips the legacy-
// layout sentinel check.
//
// MIGRATION TOOL USE ONLY — production code paths must call
// NewWithOptions, which always runs the sentinel gate. The `dfs
// migrate-to-cas` subcommand (cmd/dfs/commands/migrate_to_cas.go) is
// the sole intended caller; it must open the destination FSStore
// against a share directory that still contains the legacy `.blk`
// layout (the very state the migration is converting away from), so
// the gate would otherwise refuse the open.
//
// Behavior is otherwise identical to NewWithOptions.
func NewFSStoreForMigration(baseDir string, maxDisk, maxMemory int64, fileBlockStore block.EngineFileBlockStore, opts FSStoreOptions) (*FSStore, error) {
	return newFSStore(baseDir, maxDisk, maxMemory, fileBlockStore, opts, true)
}

// SetObjectIDPersister installs the rollup-completion callback. Safe to
// call after StartRollup has launched the rollup worker pool: read sites
// inside rollupFile take the matching RLock so the install observes a
// consistent value. The setter is idempotent — re-applying the same
// callback (or replacing it with a different one) is permitted; the
// next rollup pass uses the latest value.
//
// The typical caller is the engine's BlockStore constructor, which
// wires a closure that delegates to the metadata coordinator's
// PersistFileBlocks. Local-only fixtures may leave the persister at its
// constructor-supplied value (or nil) without invoking the setter.
// The parameter type is spelled out as a raw func value (rather than
// the local ObjectIDPersister named type) so callers that reach the
// setter through a structural interface assertion — engine.New uses an
// inline `interface { SetObjectIDPersister(func(...) error) }` to avoid
// importing fs from engine — can satisfy the interface without a
// cross-package type ceremony. The FSStoreOptions.ObjectIDPersister
// constructor slot continues to accept the named type for in-package
// callers.
func (bc *FSStore) SetObjectIDPersister(p func(ctx context.Context, payloadID string, blocks []block.BlockRef, objectID block.ObjectID) error) {
	bc.persisterMu.Lock()
	defer bc.persisterMu.Unlock()
	bc.objectIDPersister = p
}

// SetOnChunkComplete installs the chunk-completion callback after
// construction. Mirrors the SetObjectIDPersister lifecycle: callers
// (typically engine.NewBlockStore / BlockStore.Start, where the read
// Cache materializes AFTER cfg.Local was already constructed) install
// once before serving traffic. The swap is race-free — the field is
// an atomic.Pointer read by chunkstore.StoreChunk on the hot path.
// Nil is accepted: chunkstore reverts to the no-callback codepath
// (the holder is always non-nil; the inner fn may be nil).
//
// The parameter type is spelled out as a raw func value (rather than
// referring to FSStoreOptions.OnChunkComplete) so engine.go can call
// the setter through a structural interface assertion without
// importing this package's named-type spelling.
func (bc *FSStore) SetOnChunkComplete(fn func(hash block.ContentHash, data []byte, path string)) {
	bc.onChunkComplete.Store(&chunkCompleteCallback{fn: fn})
}

// SetChunkEmitter is a no-op on FSStore. FSStore drives FileBlock row
// writes through the rollup-completion persister installed via
// SetObjectIDPersister; the per-chunk emitter is only used by the
// in-memory backend whose writes don't materialize through the CAS
// chunkstore + rollup persister path. Implemented to satisfy
// [local.ChunkLifecycleHooks] so engine.New can wire all three hooks
// through a single named-interface assertion.
func (bc *FSStore) SetChunkEmitter(_ func(payloadID string, chunkStart uint64, size uint32, hash block.ContentHash)) {
}

// SetRetentionPolicy updates the retention policy and TTL for eviction decisions.
// Called during share creation and on runtime policy updates.
// Safe for concurrent use with ensureSpace/eviction (uses atomic pointer swap).
//   - pin: never evict local blocks (ensureSpace returns ErrDiskFull)
//   - ttl: evict only after file last-access exceeds ttl duration
//   - lru: evict least-recently-accessed blocks first (default)
func (bc *FSStore) SetRetentionPolicy(policy block.RetentionPolicy, ttl time.Duration) {
	cfg := &retentionConfig{policy: policy, ttl: ttl}
	atomic.StorePointer(&bc.retention, unsafe.Pointer(cfg))
}

// getRetention returns the current retention config atomically.
func (bc *FSStore) getRetention() retentionConfig {
	return *(*retentionConfig)(atomic.LoadPointer(&bc.retention))
}

// Close flushes pending FileBlock metadata and marks the store as closed.
// After Close, all read/write operations return ErrStoreClosed.
//
// Close is idempotent: multiple calls signal the background goroutine launched
// by Start() exactly once (via closeOnce), then deterministically join the
// goroutine via wg.Wait() before returning. This prevents the goroutine leak
// previously possible when Start's ctx outlived Close (a).
func (bc *FSStore) Close() error {
	// #1245 Bug C: crash-safe rollup drain on shutdown. BEFORE marking the
	// store closed (which makes rollupFile/DrainRollups short-circuit), stop
	// accepting new rollups, join the cancellable-ctx worker pool, and drain
	// any in-flight / remaining dirty payloads on a fresh, non-cancelled
	// context with a bounded grace deadline. This converts a rollup that was
	// interrupted by the cancelled runtime context — which previously surfaced
	// as a fatal context.Canceled / exit-1 — into a clean completion. A
	// drain that cannot finish within the grace window is non-fatal: the
	// append log is durable and resumes on the next boot, so we log and
	// proceed with teardown rather than failing Close. No-op when StartRollup
	// was never invoked.
	if bc.rollupStarted.Load() && !bc.isClosed() {
		// GracefulStopRollup defers a non-positive grace to its 30s default.
		if err := bc.GracefulStopRollup(bc.rollupDrainGraceDur); err != nil {
			logger.Warn("FSStore.Close: graceful rollup drain incomplete; remaining rollups resume on restart",
				"error", err)
		}
	}

	bc.closedFlag.Store(true)
	bc.closeOnce.Do(func() {
		close(bc.done)
	})
	// Join the Start goroutine (if any) before proceeding with teardown.
	// Safe to call even when Start was never invoked — Wait() on a zero
	// counter returns immediately.
	bc.wg.Wait()
	// Join rollup workers (if any were started by StartRollup). Zero-counter
	// Wait() is a no-op when the flag is off. Must run before we close log
	// fds below so no rollup goroutine touches an already-closed fd.
	bc.rollupWg.Wait()
	bc.SyncFileBlocks(context.Background())
	bc.fdPool.CloseAll()
	bc.readFDPool.CloseAll()

	// Close append-log fds. Safe after closedFlag.Store(true) +
	// wg.Wait above: no new AppendWrite can acquire an fd (isClosed guard)
	// and any rollup worker (06) has already joined via wg.
	for _, sh := range bc.logShards {
		sh.mu.Lock()
		for pid, lf := range sh.logFDs {
			if lf != nil && lf.f != nil {
				_ = lf.f.Close()
			}
			delete(sh.logFDs, pid)
		}
		sh.mu.Unlock()
	}
	return nil
}

func (bc *FSStore) isClosed() bool {
	return bc.closedFlag.Load()
}

// Start launches the background goroutine that periodically persists queued
// FileBlock metadata updates to BadgerDB. This batches many small PutFileBlock
// calls (one per 4KB NFS write) into fewer store writes (every 200ms).
//
// Must be called after New and before any writes.
// The goroutine stops on either ctx cancellation or Close() (whichever fires
// first), with a final drain on exit. Close() waits for the goroutine to
// terminate so teardown is deterministic — see a.
func (bc *FSStore) Start(ctx context.Context) {
	bc.wg.Add(1)
	go func() {
		defer bc.wg.Done()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				bc.SyncFileBlocks(context.Background())
				return
			case <-bc.done:
				bc.SyncFileBlocks(context.Background())
				return
			case <-ticker.C:
				bc.SyncFileBlocks(ctx)
			}
		}
	}()
}

// SyncFileBlocks persists all queued FileBlock metadata updates to the store.
// Called periodically by Start(), on Close(), and before GetDirtyBlocks()
// to ensure the FileBlockStore is up-to-date for ListLocalBlocks queries.
func (bc *FSStore) SyncFileBlocks(ctx context.Context) {
	bc.pendingFBs.Range(func(key, value any) bool {
		fb := value.(*block.FileBlock)
		if err := bc.blockStore.Put(ctx, fb); err == nil {
			bc.pendingFBs.Delete(key)
		}
		return true
	})
}

// SyncFileBlocksForFile persists queued FileBlock metadata only for blocks
// belonging to the given payloadID. Much cheaper than SyncFileBlocks during
// random writes to many files, since it skips unrelated blocks.
func (bc *FSStore) SyncFileBlocksForFile(ctx context.Context, payloadID string) {
	bc.pendingFBs.Range(func(key, value any) bool {
		blockID := key.(string)
		if !belongsToFile(blockID, payloadID) {
			return true
		}
		fb := value.(*block.FileBlock)
		if err := bc.blockStore.Put(ctx, fb); err == nil {
			bc.pendingFBs.Delete(key)
		}
		return true
	})
}

// diskIndexStore registers a FileBlock in the in-process diskIndex without
// queuing an async persistence. Used by Recover to seed the index from disk
// state without triggering redundant PutFileBlock writes.
func (bc *FSStore) diskIndexStore(fb *block.FileBlock) {
	bc.diskIndex.Store(fb.ID, fb)
}

// Stats returns a snapshot of current local store statistics.
func (bc *FSStore) Stats() local.Stats {
	bc.filesMu.RLock()
	fileCount := len(bc.files)
	bc.filesMu.RUnlock()

	var memBlockCount int
	bc.blocksMu.RLock()
	for _, mb := range bc.memBlocks {
		mb.mu.RLock()
		if mb.data != nil {
			memBlockCount++
		}
		mb.mu.RUnlock()
	}
	bc.blocksMu.RUnlock()

	return local.Stats{
		DiskUsed:      bc.diskUsed.Load(),
		MaxDisk:       bc.maxDisk,
		MemUsed:       bc.memUsed.Load(),
		MaxMemory:     bc.maxMemory,
		FileCount:     fileCount,
		MemBlockCount: memBlockCount,
	}
}

// updateFileSize updates the tracked file size if the new end offset is larger.
// Uses double-checked locking: RLock fast path for existing files, Lock for creation.
func (bc *FSStore) updateFileSize(payloadID string, end uint64) {
	bc.filesMu.RLock()
	fi, exists := bc.files[payloadID]
	bc.filesMu.RUnlock()

	if !exists {
		bc.filesMu.Lock()
		fi, exists = bc.files[payloadID]
		if !exists {
			fi = &fileInfo{}
			bc.files[payloadID] = fi
		}
		bc.filesMu.Unlock()
	}

	fi.mu.Lock()
	if end > fi.fileSize {
		fi.fileSize = end
	}
	fi.mu.Unlock()
}

// GetFileSize returns the tracked file size and whether the file is tracked.
// This is a fast in-memory lookup -- no disk or store access.
func (bc *FSStore) GetFileSize(_ context.Context, payloadID string) (uint64, bool) {
	bc.filesMu.RLock()
	fi, exists := bc.files[payloadID]
	bc.filesMu.RUnlock()

	if !exists {
		return 0, false
	}

	fi.mu.RLock()
	size := fi.fileSize
	fi.mu.RUnlock()

	return size, true
}

// purgeMemBlocks removes all in-memory blocks for payloadID where shouldRemove returns true.
// Releases the 8MB buffer and decrements memUsed for each removed block.
func (bc *FSStore) purgeMemBlocks(payloadID string, shouldRemove func(blockIdx uint64) bool) {
	bc.blocksMu.Lock()
	fm := bc.fileBlocks[payloadID]
	if fm != nil {
		for blockIdx, mb := range fm {
			if !shouldRemove(blockIdx) {
				continue
			}
			mb.mu.Lock()
			if mb.data != nil {
				bc.memUsed.Add(-int64(block.BlockSize))
				putBlockBuf(mb.data)
				mb.data = nil
			}
			mb.mu.Unlock()
			key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
			delete(bc.memBlocks, key)
			delete(fm, blockIdx)
		}
		if len(fm) == 0 {
			delete(bc.fileBlocks, payloadID)
		}
	}
	bc.blocksMu.Unlock()
}

// EvictMemory removes all in-memory data and disk tracking for a file.
// Does not delete .blk files from disk -- that is handled by eviction or
// explicit deletion via DeleteAllBlockFiles.
func (bc *FSStore) EvictMemory(_ context.Context, payloadID string) error {
	bc.purgeMemBlocks(payloadID, func(uint64) bool { return true })

	bc.filesMu.Lock()
	delete(bc.files, payloadID)
	bc.filesMu.Unlock()

	bc.accessTracker.Remove(payloadID)

	return nil
}

// Truncate discards local blocks beyond newSize and updates the tracked file size.
// Blocks whose start offset (blockIdx * BlockSize) >= newSize are purged from memory.
func (bc *FSStore) Truncate(_ context.Context, payloadID string, newSize uint64) error {
	bc.filesMu.RLock()
	fi, ok := bc.files[payloadID]
	bc.filesMu.RUnlock()

	if ok {
		fi.mu.Lock()
		fi.fileSize = newSize
		fi.mu.Unlock()
	}

	bc.purgeMemBlocks(payloadID, func(blockIdx uint64) bool {
		return blockIdx*block.BlockSize >= newSize
	})
	return nil
}

// ListFiles returns the payloadIDs of all files currently tracked in the local store.
func (bc *FSStore) ListFiles() []string {
	bc.filesMu.RLock()
	defer bc.filesMu.RUnlock()
	result := make([]string, 0, len(bc.files))
	for payloadID := range bc.files {
		result = append(result, payloadID)
	}
	return result
}

// belongsToFile checks if a blockID (format: "payloadID/blockIdx") belongs to
// the given payloadID by checking the prefix.
func belongsToFile(blockID, payloadID string) bool {
	if len(blockID) <= len(payloadID)+1 {
		return false
	}
	return blockID[:len(payloadID)] == payloadID && blockID[len(payloadID)] == '/'
}
