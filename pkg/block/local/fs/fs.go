package fs

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/time/rate"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/chunker"
	"github.com/marmos91/dittofs/pkg/block/local"
	"github.com/marmos91/dittofs/pkg/block/local/logblob"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// backpressureSource is the narrow, read-only window the eviction path
// consults during a remote-cache backpressure stall. It exposes ONLY the
// syncer's drain progress — never the FileChunkStore — preserving invariant
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
// interface — never the FileChunkStore — per invariant LSL-08.
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

	// errLRUEmpty is returned by blobEvictOne/compactBlobOne when there is no
	// evictable (sealed, fully-synced) blob left to reclaim.
	errLRUEmpty = errors.New("local store: LRU empty, no eviction candidates")
)

// Compile-time interface satisfaction check.
var (
	_ local.LocalStore          = (*FSStore)(nil)
	_ local.ChunkLifecycleHooks = (*FSStore)(nil)
	_ block.DurabilityReporter  = (*FSStore)(nil)
)

// ObjectIDPersister is invoked after a rollup quiesces successfully.
// It receives the payloadID, the ChunkRef manifest collected during
// chunking, and the computed ObjectID. Implementations typically
// delegate to a metadata coordinator's PersistFileChunks. The
// callback is optional; when nil, ObjectID compute still runs but
// the persist step is skipped (local-only / no-engine fixtures).
type ObjectIDPersister func(ctx context.Context, payloadID string, blocks []block.ChunkRef, objectID block.ObjectID) error

// FSStore is a two-tier (memory + disk) block store for file data.
//
// NFS WRITE operations (typically 4KB) are buffered in 8MB in-memory blocks.
// When a block fills up or NFS COMMIT is called, the block is flushed atomically
// to a .blk file on disk. This design avoids per-4KB disk I/O and prevents OS
// page cache bloat that caused OOM on earlier versions.
//
// Block metadata (local path, upload state, etc.) is tracked via FileChunkStore
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
	baseDir string
	maxDisk int64
	// the local store is one of the engine-
	// internal callers that still uses the wider EngineFileChunkStore
	// surface (GetFileChunk, ListFileChunks) on top of the narrowed
	// FileChunkStore. routes reads through FileAttr.Blocks
	// and lets us drop the wider interface entirely.
	blockStore block.EngineFileChunkStore

	// blocksMu guards the memBlocks and fileChunks maps. Uses RWMutex for
	// concurrent reads (the common case: checking if a block is already buffered).
	// RWMutex outperforms sync.Map for write-heavy workloads with high key
	// churn (random writes that create/flush/recreate blocks frequently).
	blocksMu   sync.RWMutex
	memBlocks  map[blockKey]*memBlock
	fileChunks map[string]map[uint64]*memBlock // payloadID -> blockIdx -> mb

	// filesMu guards the files map separately from block operations.
	filesMu sync.RWMutex
	files   map[string]*fileInfo

	closedFlag atomic.Bool

	memUsed  atomic.Int64
	diskUsed atomic.Int64

	// logBlob is the per-store log-blob substrate. When non-nil (a
	// localChunkIndex was wired), rolled-up chunks are appended here and
	// located via localChunkIndex instead of per-chunk cas/<hash> files.
	// Nil for index-less fixtures, which stay on the legacy CAS writer.
	logBlob *logblob.Manager
	// localChunkIndex maps content hash -> position in a log blob. Source of
	// truth for local reads of logblob-resident chunks. Nil disables the
	// logblob path (legacy CAS only).
	localChunkIndex metadata.LocalChunkIndex
	// logBlobDiskUsed accounts bytes appended to log blobs. Kept SEPARATE
	// from diskUsed so the CAS LRU eviction loop never decrements it for
	// phantom per-chunk victims; blob bytes are reclaimed whole-blob by
	// blobEvictOne, which is the only decrementer. Seeded from the physical
	// blob sizes at open (ListBlobs) so it survives restarts (#1527).
	// usedBytes() folds it into the disk-limit comparison and Stats.
	logBlobDiskUsed atomic.Int64

	// blobChunks records, per log blob, the content hashes appended during
	// this process lifetime (storeChunkLogBlob + commitStagedChunk).
	// blobEvictOne consults it to decide whether every chunk in a candidate
	// blob has been mirrored (IsSynced) and to prune the corresponding
	// local-index entries after the blob is evicted. Blobs written by a
	// previous process have no entry and fall back to the coarse global
	// "no unsynced bytes anywhere" gate; their dangling index entries are
	// dropped lazily by ReadChunk (ErrEvicted/ErrBlobNotFound → miss).
	blobChunksMu sync.Mutex
	blobChunks   map[string][]block.ContentHash

	// blobReclaimActive serializes reclaimSpace so concurrent rollup passes
	// do not stampede blob eviction. CAS-guarded try-lock: losers skip the
	// reclaim entirely (the winner is already draining to the limit).
	blobReclaimActive atomic.Bool

	// blobEvictMu serializes blobEvictOne end-to-end (candidate scan →
	// EvictBlob → counter decrement) and blobEvictedIDs records blobs this
	// store already accounted as evicted. Both exist to prevent a DOUBLE
	// DECREMENT of logBlobDiskUsed: EvictBlob is idempotent (nil for an
	// already-evicted blob), so two concurrent callers — ensureSpace on a
	// Put and reclaimSpace on a rollup worker — could otherwise both pick
	// the same sealed blob, both get nil, and both subtract its size. The
	// ID set also marks blobs whose unlink failed (evicted in-memory but
	// still on disk): those are skipped on later passes and their bytes stay
	// counted until a restart re-seeds from the physical files.
	blobEvictMu    sync.Mutex
	blobEvictedIDs map[string]struct{}

	// logBlobRollupSyncCount counts how many times a rollup pass has called
	// logBlob.Sync() before advancing the rollup_offset fence. Each
	// successful rollup pass that processes at least one chunk increments this
	// by one. Test-only observable via LogBlobRollupSyncCountForTest.
	logBlobRollupSyncCount atomic.Int64

	// flushFsyncCount counts how many times flushBlock has invoked fsync.
	// Test-only observable surfaced via FlushFsyncCountForTest. Incremented
	// only on the COMMIT-driven path (withFsync=true); pressure and
	// block-fill paths flush without fsync and do not bump this counter.
	flushFsyncCount atomic.Int64

	// pendingFBs queues FileChunk metadata updates for async persistence.
	// Keyed by blockID (string) -> *block.FileChunk.
	// Drained every 200ms by SyncFileChunks, and on Close/Flush.
	pendingFBs sync.Map

	// diskIndex is the authoritative in-process cache of FileChunk metadata
	// for blocks currently stored on the local filesystem. It is populated
	// whenever a block lands on disk (flushBlock, tryDirectDiskWrite eager-
	// create, WriteFromRemote, Recover) and pruned on delete/evict. The
	// write hot path and eviction consult diskIndex INSTEAD of the
	// FileChunkStore, decoupling those paths from the underlying metadata
	// backend.
	//
	// Entries survive pendingFBs drain so subsequent hot-path operations
	// (e.g., a second pwrite into the same block after BadgerDB persistence)
	// can still observe the block's existing State / BlockStoreKey without
	// a FileChunkStore round-trip.
	//
	// Keyed by blockID (string) -> *block.FileChunk. The stored pointer
	// is SHARED with pendingFBs so queued updates mutate a single instance.
	diskIndex sync.Map

	// verified tracks CAS chunk hashes already BLAKE3-verified this process, so
	// the warm read path skips re-hashing the whole covering chunk on every
	// read of an immutable chunk (see verified_chunks.go / chunkTrusted).
	verified *verifiedChunkSet

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

	// durable reports whether bytes accepted by this store survive a crash /
	// restart (block.DurabilityReporter). The fs backend writes to disk and
	// never evicts un-mirrored chunks, so the type default is true; an operator
	// may override it via the per-store config["durable"] bool (SetDurable),
	// e.g. for a tmpfs-backed share where the disk is volatile.
	durable atomic.Bool

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

	// syncEveryWrite, when true, makes AppendWrite fsync the append log
	// inline on every record (the pre-PR3 behavior). The default (false)
	// honors NFS UNSTABLE: AppendWrite leaves the record in the page cache
	// and the fsync is deferred to the next durability point (COMMIT /
	// DATA_SYNC/FILE_SYNC WRITE / SMB Flush / graceful drain), each of which
	// routes through SyncPayload. Operators mount clients that never issue
	// COMMIT can opt back into per-write fsync via config["sync_every_write"].
	syncEveryWrite bool

	// chunkParams is the FastCDC sizing used by the rollup chunker (#1569).
	// Lowering Min shrinks chunks to cut random-read amplification on this
	// share; the zero value falls back to chunker.DefaultParams() (1M/4M/16M).
	// Write-time only — reads never re-chunk (the FileChunk manifest freezes
	// boundaries), so a share may change this and still read its older data.
	chunkParams chunker.Params

	// syncLeader batches every append-log fd fsync (inline SyncEveryWrite
	// writes and SyncPayload COMMITs alike) into as few journal commits as
	// possible. One per store so concurrent fds coalesce (PR3 / #1416).
	syncLeader *syncLeader

	// logFsyncCount counts fsyncs issued against append-log fds through the
	// syncLeader (SyncEveryWrite writes + SyncPayload). Test-only observable
	// via LogFsyncCountForTest, used to assert the deferred/COMMIT fsync
	// accounting. Not incremented on the deferred UNSTABLE write path.
	logFsyncCount atomic.Int64

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
	// semantics. NEVER hand the FileChunkStore here: this is a narrow,
	// non-FileChunkStore interface, preserving invariant LSL-08.
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

	// scanning elects a single ticker-driven backlog scan at a time. Every
	// rollup worker tickers at the same stabilization interval; without this
	// guard all of them would fan out over the same dirty set and collide on
	// per-file rmu (correct, but pure waste). The elected scanner fans the
	// backlog out across the worker budget itself (#1411 scanAllFiles).
	scanning atomic.Bool

	// rollupSlots is the shared rollup concurrency budget. BOTH the rollupCh
	// worker pool (nudge path) and the scanAllFiles backlog fan-out acquire a
	// slot before calling rollupFile, so rollup_workers caps the TOTAL number
	// of concurrent rollups rather than each path independently (which would
	// let write+tick overlap reach ~2x the configured budget). Buffered to
	// rollup_workers; send = acquire, receive = release.
	rollupSlots chan struct{}

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
	// rollupFile passes the chunker's accumulated ChunkRef manifest and
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

	// ---: chunk-complete hook. ---

	// onChunkComplete fires once per successful chunkstore.StoreChunk.
	// Install via
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
	// (a) its rollup_offset in metadata is 0, (b) no FileChunk exists for
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
func newFSStore(baseDir string, maxDisk int64, fileChunkStore block.EngineFileChunkStore, opts FSStoreOptions, skipSentinelCheck bool) (*FSStore, error) {
	if !skipSentinelCheck {
		if err := checkLegacyLayoutSentinel(baseDir); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("local store: create base dir: %w", err)
	}

	defaultRetention := &retentionConfig{policy: block.RetentionLRU}
	bc := &FSStore{
		baseDir:       baseDir,
		maxDisk:       maxDisk,
		blockStore:    fileChunkStore,
		memBlocks:     make(map[blockKey]*memBlock),
		fileChunks:    make(map[string]map[uint64]*memBlock),
		files:         make(map[string]*fileInfo),
		fdPool:        newFDPool(defaultFDPoolSize),
		readFDPool:    newFDPool(defaultFDPoolSize),
		verified:      newVerifiedChunkSet(verifiedChunkCap),
		accessTracker: newAccessTracker(),
		retention:     unsafe.Pointer(defaultRetention),
		done:          make(chan struct{}),
		stopRollup:    make(chan struct{}),
	}
	bc.evictionEnabled.Store(true)
	// Type default: fs-backed local storage is durable (#1274). Overridable
	// post-construction via SetDurable from the controlplane config["durable"].
	bc.durable.Store(true)

	bc.blobChunks = make(map[string][]block.ContentHash)
	bc.blobEvictedIDs = make(map[string]struct{})

	// append-log plumbing — maps + pressure channel are always
	// initialized; the opt-out flag was deleted with the legacy
	// path-keyed writer.
	bc.pressureCh = make(chan struct{}, 1)
	bc.syncLeader = newSyncLeader()
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

	// LocalChunkIndex is mandatory: chunk persistence routes exclusively to
	// the log-blob substrate + index. Production and all fixtures wire a
	// metadata backend (badger/sqlite/postgres/memory) that implements it.
	lci, ok := fileChunkStore.(metadata.LocalChunkIndex)
	if !ok {
		return nil, fmt.Errorf("fs local store: metadata backend %T must implement metadata.LocalChunkIndex", fileChunkStore)
	}
	bc.localChunkIndex = lci

	applyFSStoreOptions(bc, opts)

	// Open the per-store log-blob substrate at <baseDir>/blobs/. Rolled-up
	// chunks append here (located via the index).
	mgr, err := logblob.Open(filepath.Join(baseDir, "blobs"), logblob.Options{})
	if err != nil {
		return nil, fmt.Errorf("local store: open log-blob substrate: %w", err)
	}
	bc.logBlob = mgr
	// Seed the blob byte counter from the physical blob files so the
	// disk-usage figure survives restarts (#1527). ListBlobs reports the
	// active blob at its recovered tail and sealed blobs at file size.
	infos, err := mgr.ListBlobs()
	if err != nil {
		return nil, fmt.Errorf("local store: seed log-blob disk usage: %w", err)
	}
	var blobBytes int64
	for _, bi := range infos {
		blobBytes += bi.Size
	}
	bc.logBlobDiskUsed.Store(blobBytes)

	// Shared rollup concurrency budget, sized to the final worker count so
	// the nudge path and the scanAllFiles fan-out share it (#1411). Floor of
	// 1 guards the degenerate rollupWorkers<=0 path.
	slots := bc.rollupWorkers
	if slots <= 0 {
		slots = 2
	}
	bc.rollupSlots = make(chan struct{}, slots)
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
	bc.syncEveryWrite = opts.SyncEveryWrite
	bc.chunkParams = opts.ChunkParams
	if bc.chunkParams.Validate() != nil {
		bc.chunkParams = chunker.DefaultParams()
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

// FSStoreOptions configures the append-log path. Append is mandatory in
// — the UseAppendLog opt-out flag was deleted with the legacy
// path-keyed writer. MaxLogBytes, RollupWorkers, and StabilizationMS
// default via NewWithOptions when left zero here.
type FSStoreOptions struct {
	MaxLogBytes     int64
	RollupWorkers   int
	StabilizationMS int
	// ChunkParams sets the FastCDC sizing for this share's rollup chunker
	// (#1569). The zero value falls back to chunker.DefaultParams() (1M/4M/16M,
	// byte-identical to pre-#1569). A random-access share lowers ChunkParams.Min
	// (and Max) to shrink chunks and cut random-read amplification.
	ChunkParams chunker.Params

	// SyncEveryWrite forces AppendWrite to fsync the append log on every
	// record. Default false honors NFS UNSTABLE — the fsync is deferred to
	// the next durability point (COMMIT / DATA_SYNC/FILE_SYNC WRITE / SMB
	// Flush / graceful drain), which is where the ~3x sequential-write win
	// comes from. Set true (config["sync_every_write"]) only for clients that
	// never issue COMMIT and expect per-write durability. See
	// FSStore.syncEveryWrite.
	SyncEveryWrite bool
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
	// ChunkRef manifest + computed ObjectID after SetRollupOffset
	// succeeds. Wire this to the engine coordinator's PersistFileChunks
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

	// OnChunkComplete is invoked once per successful chunk store
	// (storeChunkLogBlob / stageRollupChunk), with the chunk's content hash and
	// bytes (the path argument is empty on the log-blob path). Wire to
	// engine.Cache.Put to populate the read cache at write time. Nil is
	// accepted: chunkstore behaves identically to the pre-callback path if
	// absent. Callback MUST be non-blocking on hot paths.
	OnChunkComplete func(hash block.ContentHash, data []byte, path string)

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
//   - fileChunkStore: persistent store for FileChunk metadata
//     (local path, upload state, etc.).
//   - opts: tunables for append-log sizing, rollup pool, dedup LRU,
//     chunk-complete hook, etc. Zero-valued fields fall back to the
//     defaults documented on FSStoreOptions.
//
// Runs the legacy-layout sentinel gate; returns
// block.ErrLegacyLayoutDetected when the share holds the pre-CAS
// `.blk` layout without a `.cas-migrated-v1` marker.
func NewWithOptions(baseDir string, maxDisk int64, fileChunkStore block.EngineFileChunkStore, opts FSStoreOptions) (*FSStore, error) {
	return newFSStore(baseDir, maxDisk, fileChunkStore, opts, false)
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
func NewFSStoreForMigration(baseDir string, maxDisk int64, fileChunkStore block.EngineFileChunkStore, opts FSStoreOptions) (*FSStore, error) {
	return newFSStore(baseDir, maxDisk, fileChunkStore, opts, true)
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
// PersistFileChunks. Local-only fixtures may leave the persister at its
// constructor-supplied value (or nil) without invoking the setter.
// The parameter type is spelled out as a raw func value (rather than
// the local ObjectIDPersister named type) so callers that reach the
// setter through a structural interface assertion — engine.New uses an
// inline `interface { SetObjectIDPersister(func(...) error) }` to avoid
// importing fs from engine — can satisfy the interface without a
// cross-package type ceremony. The FSStoreOptions.ObjectIDPersister
// constructor slot continues to accept the named type for in-package
// callers.
func (bc *FSStore) SetObjectIDPersister(p func(ctx context.Context, payloadID string, blocks []block.ChunkRef, objectID block.ObjectID) error) {
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

// SetChunkEmitter is a no-op on FSStore. FSStore drives FileChunk row
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

// Close flushes pending FileChunk metadata and marks the store as closed.
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
	// The stop/drain + closed-flag + done-signal sequence runs exactly once,
	// even under concurrent Close() calls: closeOnce serializes it so two
	// callers can never race two drains against each other (the second blocks
	// in Do until the first finishes, then falls through to the idempotent
	// teardown below).
	bc.closeOnce.Do(func() {
		if bc.rollupStarted.Load() {
			// GracefulStopRollup defers a non-positive grace to its 30s default.
			if err := bc.GracefulStopRollup(bc.rollupDrainGraceDur); err != nil {
				logger.Warn("FSStore.Close: graceful rollup drain incomplete; remaining rollups resume on restart",
					"error", err)
			}
		}
		bc.closedFlag.Store(true)
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
	bc.SyncFileChunks(context.Background())
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

	// Close the log-blob substrate (if wired). Safe after the rollup workers
	// have joined above, so no Append/ReadAt is in flight.
	if bc.logBlob != nil {
		if err := bc.logBlob.Close(); err != nil {
			logger.Warn("FSStore.Close: log-blob close failed", "error", err)
		}
	}
	return nil
}

func (bc *FSStore) isClosed() bool {
	return bc.closedFlag.Load()
}

// Start launches the background goroutine that periodically persists queued
// FileChunk metadata updates to BadgerDB. This batches many small PutFileChunk
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
				bc.SyncFileChunks(context.Background())
				return
			case <-bc.done:
				bc.SyncFileChunks(context.Background())
				return
			case <-ticker.C:
				bc.SyncFileChunks(ctx)
			}
		}
	}()
}

// SyncFileChunks persists all queued FileChunk metadata updates to the store.
// Called periodically by Start(), on Close(), and before GetDirtyBlocks()
// to ensure the FileChunkStore is up-to-date for ListLocalBlocks queries.
func (bc *FSStore) SyncFileChunks(ctx context.Context) {
	bc.pendingFBs.Range(func(key, value any) bool {
		fb := value.(*block.FileChunk)
		if err := bc.blockStore.Put(ctx, fb); err == nil {
			bc.pendingFBs.Delete(key)
		}
		return true
	})
}

// SyncFileChunksForFile persists queued FileChunk metadata only for blocks
// belonging to the given payloadID. Much cheaper than SyncFileChunks during
// random writes to many files, since it skips unrelated blocks.
func (bc *FSStore) SyncFileChunksForFile(ctx context.Context, payloadID string) {
	bc.pendingFBs.Range(func(key, value any) bool {
		blockID := key.(string)
		if !belongsToFile(blockID, payloadID) {
			return true
		}
		fb := value.(*block.FileChunk)
		if err := bc.blockStore.Put(ctx, fb); err == nil {
			bc.pendingFBs.Delete(key)
		}
		return true
	})
}

// diskIndexStore registers a FileChunk in the in-process diskIndex without
// queuing an async persistence. Used by Recover to seed the index from disk
// state without triggering redundant PutFileChunk writes.
func (bc *FSStore) diskIndexStore(fb *block.FileChunk) {
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
		// DiskUsed covers the store's data files: legacy CAS chunks + .blk
		// block files (diskUsed) plus log-blob bytes (logBlobDiskUsed).
		// Append-log bytes (logs/*.log) are transient rollup input governed
		// by the separate MaxLogBytes budget and are intentionally excluded.
		DiskUsed:      bc.usedBytes(),
		MaxDisk:       bc.maxDisk,
		MaxLogBytes:   bc.maxLogBytes,
		MemUsed:       bc.memUsed.Load(),
		FileCount:     fileCount,
		MemBlockCount: memBlockCount,
	}
}

// usedBytes returns the store's total accounted on-disk data footprint:
// legacy CAS chunk files + .blk block files (diskUsed) plus log-blob bytes
// (logBlobDiskUsed). This is the figure ensureSpace and reclaimSpace compare
// against maxDisk, and what Stats reports as DiskUsed (#1527).
func (bc *FSStore) usedBytes() int64 {
	return bc.diskUsed.Load() + bc.logBlobDiskUsed.Load()
}

// subUsed subtracts delta from the given usage counter, clamping at zero.
// An attempted underflow means asymmetric accounting — bytes were removed
// that were never added — and is logged at Error: the counter gates
// eviction/backpressure, so a silently negative value disables the disk
// limit entirely (#1527: dittofs_localstore_disk_used_bytes went negative
// and the local tier grew unbounded).
func (bc *FSStore) subUsed(c *atomic.Int64, delta int64, counter string) {
	if delta <= 0 {
		return
	}
	for {
		cur := c.Load()
		next := cur - delta
		if next < 0 {
			logger.Error("local store: disk-used counter underflow, clamping to 0",
				"counter", counter, "current", cur, "delta", delta, "dir", bc.baseDir)
			next = 0
		}
		if c.CompareAndSwap(cur, next) {
			return
		}
	}
}

// trackBlobChunk records that chunk h was appended to blob blobID in this
// process lifetime. blobEvictOne uses the recorded set for its per-blob
// synced check and post-evict index cleanup. Zero-length chunks carry an
// empty blob ID and are not tracked (no blob bytes to reclaim).
func (bc *FSStore) trackBlobChunk(blobID string, h block.ContentHash) {
	if blobID == "" {
		return
	}
	bc.blobChunksMu.Lock()
	bc.blobChunks[blobID] = append(bc.blobChunks[blobID], h)
	bc.blobChunksMu.Unlock()
}

// MaxLogBytes returns the effective append-log pressure budget in bytes
// (the resolved max_log_bytes: per-store > global > deduced default).
// AppendWrite blocks once logBytesTotal exceeds this ceiling and ultimately
// returns block.ErrPressureTimeout if the rollup cannot drain in time. The
// value is immutable after construction, so it is read without a lock.
func (bc *FSStore) MaxLogBytes() int64 { return bc.maxLogBytes }

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
	fm := bc.fileChunks[payloadID]
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
			delete(bc.fileChunks, payloadID)
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
