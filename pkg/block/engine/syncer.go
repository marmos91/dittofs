package engine

import (
	"context"
	"errors"
	"fmt"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// defaultShutdownTimeout is the maximum time to wait for the transfer queue
// to finish processing during graceful shutdown.
const defaultShutdownTimeout = 30 * time.Second

// fetchResult is a broadcast-capable result for in-flight download deduplication.
// When the download completes, err is set and done is closed. Multiple waiters can
// safely read the result because closing a channel notifies ALL receivers.
type fetchResult struct {
	done chan struct{} // Closed when download completes
	err  error         // Result of the download (set before closing done)
	mu   gosync.Mutex  // Protects err during write
}

// Syncer handles async local-to-remote transfers with eager block carving,
// parallel download, prefetch, in-flight dedup, and content-addressed dedup.
type Syncer struct {
	local       local.LocalStore
	remoteStore remote.RemoteStore
	// hasRemote mirrors "remoteStore != nil" as an atomic so hot-path gating
	// (carveActive recompute, read from addPendingHash under pendingMu) can
	// read it without taking m.mu — avoiding both a data race with
	// SetRemoteStore and a pendingMu→m.mu lock-ordering edge.
	hasRemote atomic.Bool
	// the syncer is one of the engine-internal
	// callers that still reaches into the wider EngineFileChunkStore
	// surface (GetFileChunk for dual-read resolve, ListFileChunks for
	// GetFileSize/Exists). routes reads through
	// FileAttr.Blocks and lets us drop the wider interface.
	fileChunkStore block.EngineFileChunkStore // Required: enables content-addressed deduplication

	// syncedHashStore persists per-CAS-hash local→remote sync state. The
	// restart/drift reseed consumes local.ListUnsynced (which itself filters
	// via SyncedHashStore.IsSynced); the carver records synced markers +
	// block locators atomically via DefaultCommitBlock. May be nil in unit
	// tests / local-only fixtures; production callers wire a real store via
	// SetSyncedHashStore.
	syncedHashStore metadata.SyncedHashStore

	// bs is a back-reference to the owning Store.
	// the file-level dedup short-circuit needs to reach
	// Store.cache to fire InvalidateFile on orphaned speculative
	// chunks. Reading through the back-reference (rather than copying a
	// `cache` field on the Syncer at construction time) lets test code
	// swap `bs.cache = rec` after construction and still observe the
	// invalidation — mirrors the TestClose_ClosesCache pattern. May be
	// nil in pre-wiring tests; callers must nil-check before use.
	bs *Store

	config SyncerConfig

	queue *SyncQueue // Transfer queue for non-blocking operations

	inFlight   map[string]*fetchResult // In-flight download dedup (store key -> broadcast)
	inFlightMu gosync.Mutex

	// readahead holds per-payload sequential-access state so remote prefetch
	// ramps on sequential runs and backs off on random access (see readahead.go).
	readahead   map[string]*raState
	readaheadMu gosync.Mutex

	stopCh chan struct{} // Signals periodic uploader to stop
	closed bool
	mu     gosync.RWMutex

	periodicStarted bool // true once the carve dispatcher + maintenance goroutines are launched

	healthMonitor   *HealthMonitor           // Monitors remote store health (nil when no remote)
	onHealthChanged healthTransitionCallback // Callback invoked on health state transitions

	firstOfflineRead    atomic.Bool  // Tracks if WARN was already logged since last healthy->unhealthy transition
	offlineReadsBlocked atomic.Int64 // Count of read operations blocked by remote unavailability

	// pendingMu guards the carve pending set (pendingCarveHashes + carveQ).
	pendingMu gosync.Mutex

	// unsyncedBytes is the running total on-disk size of pendingCarveHashes:
	// the number of cache bytes that cannot be evicted until they reach
	// remote. The local store reads it (via UnsyncedBytes) to decide
	// whether a backpressure stall can make progress. Charged once per
	// distinct hash (CAS dedup): re-adding a hash already pending does not
	// double-count, and a drained hash subtracts exactly what it added.
	unsyncedBytes atomic.Int64

	// completedSyncs / failedSyncs are lifetime counters of CAS chunks that
	// reached remote (committed inside a packed block) and of failed carve
	// upload attempts. They source the truthful CompletedSyncs / FailedSyncs
	// fields in block stats; the legacy SyncQueue has no production upload
	// callers, so its counters always read zero (#1266).
	completedSyncs atomic.Int64
	failedSyncs    atomic.Int64

	// uploadLimiter bounds concurrent block PUTs in carveFlush (#1407 / #1432).
	// When ParallelUploads is pinned (> 0) its limit is fixed at that value.
	// When unset (adaptive mode) the uploadController resizes it every control
	// interval to track the goodput knee. Lazily created by ensureUploadLimiter
	// so directly-built test fixtures still get bounded concurrency.
	uploadLimiter *dynamicSemaphore
	// uploadController is non-nil only in adaptive mode. It consumes one
	// (goodput, windowLimited, sawError) sample per control interval and returns
	// the next target window, applied to uploadLimiter by the control goroutine.
	uploadController *goodputController
	// uploadedBytesWindow accumulates bytes successfully PutBlock'd since the
	// last control tick; uploadErrWindow counts block-upload errors in the same
	// span. The control goroutine swaps both to zero each tick to compute the
	// goodput sample and the error flag. Plain atomics — no lock needed.
	uploadedBytesWindow atomic.Int64
	uploadErrWindow     atomic.Int64

	// --- block carve path (#1414 object packing) ---

	// remoteBlockStore is the block-keyed remote (PutBlock) the carver uploads
	// packed blocks to. nil disables carve. Wired via SetRemoteBlockStore;
	// guarded by m.mu.
	remoteBlockStore remote.RemoteBlockStore
	// chunkSealer applies the per-chunk compression/encryption transform before
	// a chunk is framed into a block. Derived from remoteBlockStore (the same
	// decorated remote); nil means identity (raw) sealing. Guarded by m.mu.
	chunkSealer remote.ChunkSealer
	// blockCommitter atomically persists the block record + local locations +
	// synced markers (DefaultCommitBlock) and resolves a chunk's log-blob
	// location (GetLocalLocation). It is the per-share metadata store; nil
	// disables carve. Guarded by m.mu.
	blockCommitter blockCommitter
	// localBlobReader reads a chunk's raw bytes from the local log-blob
	// substrate (FSStore). nil disables carve. Asserted from m.local at
	// construction; guarded by m.mu only for read symmetry with the others.
	localBlobReader localBlobReader

	// carveActive mirrors "all carve deps wired AND a remote exists" as an
	// atomic so hot paths (Flush honesty check, carveFlush early-out) can read
	// it without taking m.mu. Recomputed by the setters.
	carveActive atomic.Bool

	// pendingCarveHashes holds chunks awaiting carve into a block, keyed by
	// hash → raw byte size. Populated O(1) by addPendingHash (fired from the
	// onChunkComplete chokepoint) and drained by carveAndCommitBlock after
	// each atomic commit. A startup reconciliation (seedPendingFromDisk)
	// re-seeds it after a restart, since the set is volatile and chunks
	// written-but-not-synced before a crash would otherwise be missed. carveQ
	// is the FIFO insertion order the carver drains. Both guarded by
	// pendingMu. The per-hash size feeds unsyncedBytes, the backpressure
	// signal the local store consults to decide whether to keep stalling a
	// writer.
	pendingCarveHashes map[block.ContentHash]int64
	carveQ             []block.ContentHash

	// carveWake nudges the carve dispatcher that a chunk is ready (non-blocking,
	// coalescing, buffered len 1) so packing overlaps later writes.
	carveWake chan struct{}
	// carveMu serializes carve flushes so the background dispatcher and an
	// explicit Flush/SyncNow never build a block from the same chunk twice.
	carveMu gosync.Mutex
}

// blockCommitter is the narrow consumer-side slice of metadata.Store the carver
// needs: transactional block-record commit (DefaultCommitBlock takes a
// Transactor+SyncedHashStore), the synced-marker writes it performs, and the
// local-chunk-index lookup that resolves a hash to its log-blob position. The
// production per-share metadata store satisfies all three; defining it here
// keeps the engine off the wider metadata.Store surface.
type blockCommitter interface {
	metadata.Transactor
	metadata.SyncedHashStore
	metadata.LocalChunkIndex
}

// localBlobReader is the narrow consumer-side capability the carver needs from
// the local store: read a chunk's raw bytes from the log-blob substrate at a
// known location. *fs.FSStore satisfies it via a thin delegation to its
// log-blob Manager. Kept off local.LocalStore so non-log-blob backends compile
// unchanged (the carver is simply disabled for them).
type localBlobReader interface {
	ReadLocalAt(ctx context.Context, loc block.LocalChunkLocation, dst []byte) (int, error)
}

// addPendingHash registers a newly-stored CAS hash (of the given on-disk
// byte size) for the next carve pass. Fired from the onChunkComplete
// callback (engine.New) on every successful StoreChunk. Safe for concurrent
// use; O(1). Charges unsyncedBytes once per distinct hash — re-adding a hash
// already pending updates the recorded size but does not double-count.
// Harmless when no remote is configured: the set simply accumulates until a
// remote is attached (SetRemoteStore) or the chunks are marked synced.
// Signals the carve dispatcher so packing overlaps the rollup of later chunks.
func (m *Syncer) addPendingHash(h block.ContentHash, size int64) {
	m.pendingMu.Lock()
	// prev is the zero value (0) when the hash is new, so size-prev charges
	// the full size on first insert and only the delta on re-add — never
	// double-counting a hash already pending (CAS dedup). The counter update
	// stays INSIDE pendingMu so it is serialized against the carve drain
	// (which deletes from the map and subtracts under the same lock).
	prev, already := m.pendingCarveHashes[h]
	m.pendingCarveHashes[h] = size
	m.unsyncedBytes.Add(size - prev)
	if !already {
		m.carveQ = append(m.carveQ, h)
	}
	m.pendingMu.Unlock()

	// Only a newly-inserted hash changes the queue depth; re-adding an
	// already-pending hash (size refresh) leaves the gauge unchanged, so skip
	// the extra lock/metric churn on that hot path (incl. seedPendingFromDisk).
	if !already {
		m.publishCarveQueueDepth()
		m.signalCarveWake()
	}
}

// markFetchedSynced retires a chunk that was just downloaded from the remote
// store from the pending-carve set. The bytes are verbatim remote content —
// the chunk was resolved through its recorded block locator, so it is already
// marked synced and provably durable on remote. All that remains is to cancel
// the pending entry that StoreChunk's onChunkComplete callback registered for
// it, so the carver does not waste a redundant block commit re-packing data
// that is already remote (read-amplification → write-amplification, #1362).
//
// It deliberately does NOT call MarkSynced: the hash already carries a block
// locator, and re-marking it here would overwrite that locator (the legacy
// path recorded a standalone locator, which is post-migration drift).
func (m *Syncer) markFetchedSynced(_ context.Context, h block.ContentHash) {
	if h.IsZero() {
		return
	}
	// A concurrent carve pass that already claimed this hash may still pack it
	// once — harmless and rare (the commit is idempotent per MarkSynced).
	m.dropCarveHash(h)
	m.publishCarveQueueDepth()
}

// UnsyncedBytes returns the running total on-disk size of CAS chunks present
// locally but not yet mirrored to remote. This is the backpressure signal
// the local store consults: a non-zero value with a healthy remote means a
// stalled writer can make progress once the syncer drains. The raw counter
// can briefly go negative when a drift reconcile re-seeds a still-pending
// hash with a best-effort size of 0 (its bytes vanished mid-walk); this
// method clamps such a transient to 0 so callers always see a non-negative
// pending-byte count.
func (m *Syncer) UnsyncedBytes() int64 {
	if v := m.unsyncedBytes.Load(); v > 0 {
		return v
	}
	return 0
}

// seedPendingFromDisk reconciles the in-memory pending-carve set against the
// on-disk state by walking every locally-present chunk that is not yet
// marked synced (ListUnsynced) and adding it to the set. This is the
// O(total-chunks) walk — but it runs ONCE at startup (the pending set is
// volatile, so chunks written-but-not-synced before a crash must be
// rediscovered) and periodically as a slow drift reconciler, NOT on every
// carve tick. Returns the number of hashes seeded.
//
// Restart-seed coverage of unsynced LOG-BLOB chunks is not yet guaranteed:
// ListUnsynced enumerates log-blob chunks only on index backends that implement
// local-location walking, so on the production backends an unsynced log-blob
// chunk written just before a crash is not re-queued for carve here. The chunk
// is durable locally (its blob is fsynced before the rollup fence advances), so
// no data is lost; full cross-backend restart-seed via blob-index enumeration
// is deferred to the #1525 reconcile work (PR5).
func (m *Syncer) seedPendingFromDisk(ctx context.Context) (int, error) {
	n := 0
	for hash, err := range m.local.ListUnsynced(ctx) {
		if err != nil {
			return n, fmt.Errorf("seed pending: %w", err)
		}
		// Recover each unsynced chunk's on-disk size so unsyncedBytes is
		// accurate after a restart. A chunk that vanished between the
		// ListUnsynced walk and this Head (external delete / concurrent
		// evict) is recorded as zero bytes rather than aborting the seed —
		// the next drift reconcile re-walks disk and corrects the set.
		var size int64
		if meta, herr := m.local.Head(ctx, hash); herr == nil {
			size = meta.Size
		}
		m.addPendingHash(hash, size)
		n++
	}
	return n, nil
}

// NewSyncer creates a new Syncer. The fileChunkStore is required for content-addressed dedup.
func NewSyncer(local local.LocalStore, remoteStore remote.RemoteStore, fileChunkStore block.EngineFileChunkStore, config SyncerConfig) *Syncer {
	if fileChunkStore == nil {
		panic("fileChunkStore is required for Syncer")
	}
	if config.ParallelDownloads <= 0 {
		config.ParallelDownloads = DefaultParallelDownloads
	}
	if config.PrefetchBlocks <= 0 {
		config.PrefetchBlocks = DefaultPrefetchBlocks
	}
	// — apply CAS-path defaults.
	if config.ClaimTimeout <= 0 {
		config.ClaimTimeout = 10 * time.Minute
	}

	// Upload concurrency (#1407 / #1432): a pinned ParallelUploads > 0 fixes the
	// window; otherwise (the default) the carver auto-tunes between the adaptive
	// floor and ceiling. The limiter starts at the floor in adaptive mode and at
	// the pinned value otherwise; the control goroutine (adaptive only, launched
	// in Start) resizes it at runtime.
	var uploadController *goodputController
	startWindow := config.ParallelUploads
	if config.ParallelUploads <= 0 {
		startWindow = AdaptiveUploadFloor
		uploadController = newGoodputController(AdaptiveUploadFloor, AdaptiveUploadCeiling)
	}

	m := &Syncer{
		local:          local,
		remoteStore:    remoteStore,
		fileChunkStore: fileChunkStore,
		config:         config,
		inFlight:       make(map[string]*fetchResult),
		readahead:      make(map[string]*raState),
		stopCh:         make(chan struct{}),

		uploadLimiter:    newDynamicSemaphore(startWindow),
		uploadController: uploadController,

		pendingCarveHashes: make(map[block.ContentHash]int64),
		carveWake:          make(chan struct{}, 1),
	}
	m.hasRemote.Store(remoteStore != nil)
	// The local store provides the carve read path only if it exposes the
	// log-blob substrate; non-log-blob backends leave the carver disabled.
	if r, ok := local.(localBlobReader); ok {
		m.localBlobReader = r
	}
	m.recomputeCarveActive()

	queueConfig := DefaultSyncQueueConfig()
	queueConfig.DownloadWorkers = config.ParallelDownloads
	m.queue = NewSyncQueue(m, queueConfig)

	return m
}

// Queue returns the transfer queue for stats inspection.
func (m *Syncer) Queue() *SyncQueue { return m.queue }

// SetSyncedHashStore wires the per-hash sync-state store the restart/drift
// reseed consults via local.ListUnsynced and the carver updates through
// DefaultCommitBlock. Idempotent. May be invoked after NewSyncer so the
// construction sequence does not need to thread a SyncedHashStore through
// the engine.NewSyncer signature.
func (m *Syncer) SetSyncedHashStore(s metadata.SyncedHashStore) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncedHashStore = s
	// The same per-share metadata store backs the block carver's atomic commit
	// (DefaultCommitBlock) and log-blob location lookup. Derive blockCommitter
	// here so the carve wiring rides the existing SetSyncedHashStore call; a
	// store that is only a bare SyncedHashStore (test fixture) leaves carve
	// disabled.
	if bc, ok := s.(blockCommitter); ok {
		m.blockCommitter = bc
	} else {
		m.blockCommitter = nil
	}
	m.recomputeCarveActive()
}

// SetRemoteBlockStore wires the block-keyed remote (PutBlock) the carver
// uploads packed blocks to, and derives the per-chunk ChunkSealer from the same
// (possibly decorated) remote. Placed beside SetSyncedHashStore in the wiring
// sequence. A nil rbs — or a remote that does not implement RemoteBlockStore —
// leaves carve disabled: pending chunks then accumulate locally and Flush
// reports Finalized=false (there is no legacy per-hash fallback). Idempotent;
// safe to call before Start.
func (m *Syncer) SetRemoteBlockStore(rbs remote.RemoteBlockStore) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.remoteBlockStore = rbs
	if cs, ok := rbs.(remote.ChunkSealer); ok {
		m.chunkSealer = cs
	} else {
		m.chunkSealer = nil
	}
	m.recomputeCarveActive()
}

// recomputeCarveActive refreshes the carveActive routing flag from the carve
// dependencies. A log-blob reader is deliberately NOT required: local stores
// without a log-blob substrate (memory) carve through the hash-keyed local
// read fallback (carveChunkBytes). Caller must hold m.mu (or be the
// single-threaded constructor).
// Carve routing does NOT gate on ManualSync: in manual mode the background
// carver is suppressed but explicit Flush/SyncNow still drains the carve set,
// so log-blob chunks must still route to it.
func (m *Syncer) recomputeCarveActive() {
	active := m.remoteBlockStore != nil &&
		m.blockCommitter != nil &&
		m.hasRemote.Load()
	m.carveActive.Store(active)
}

// SetHealthCallback sets the callback invoked when the remote store health state changes.
// If the HealthMonitor is already running, the callback is forwarded to it immediately.
func (m *Syncer) SetHealthCallback(fn healthTransitionCallback) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onHealthChanged = fn
	if m.healthMonitor != nil {
		m.healthMonitor.SetTransitionCallback(fn)
	}
}

// IsRemoteHealthy returns the health state of the remote store.
// Returns true when there is no HealthMonitor (local-only mode).
func (m *Syncer) IsRemoteHealthy() bool {
	if m.healthMonitor == nil {
		return true
	}
	return m.healthMonitor.IsHealthy()
}

// RemoteOutageDuration returns how long the remote store has been unhealthy.
// Returns 0 when healthy or when there is no HealthMonitor.
func (m *Syncer) RemoteOutageDuration() time.Duration {
	if m.healthMonitor == nil {
		return 0
	}
	return m.healthMonitor.OutageDuration()
}

// remoteUnavailableError returns an ErrRemoteUnavailable wrapped with outage duration context.
func (m *Syncer) remoteUnavailableError() error {
	dur := m.RemoteOutageDuration()
	return fmt.Errorf("remote store unavailable (offline for %s): %w", dur.Truncate(time.Second), block.ErrRemoteUnavailable)
}

// OfflineReadsBlocked returns the count of read operations that failed
// because the requested blocks were remote-only during an outage.
func (m *Syncer) OfflineReadsBlocked() int64 {
	return m.offlineReadsBlocked.Load()
}

// logOfflineRead logs a read failure due to remote unavailability.
// First failure after a healthy->unhealthy transition logs at WARN level
// subsequent failures log at DEBUG to avoid log spam.
func (m *Syncer) logOfflineRead(method, payloadID string, blockIdx uint64) {
	if m.firstOfflineRead.CompareAndSwap(false, true) {
		logger.Warn("Read blocked: remote store unavailable",
			"method", method,
			"payloadID", payloadID,
			"blockIdx", blockIdx,
			"outage_duration", m.RemoteOutageDuration().Truncate(time.Second))
	} else {
		logger.Debug("Read blocked: remote store unavailable",
			"method", method,
			"payloadID", payloadID,
			"blockIdx", blockIdx)
	}
}

// checkReady returns nil if the syncer can process requests.
// Returns ctx.Err() if the context is cancelled, or ErrClosed if the syncer is closed.
func (m *Syncer) checkReady(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return ErrClosed
	}
	return nil
}

// canProcess returns false if the syncer is closed or context is cancelled.
func (m *Syncer) canProcess(ctx context.Context) bool {
	return m.checkReady(ctx) == nil
}

// Flush quiesces a payload's local-side state and drains the pending-carve
// set: every locally stored chunk that has not yet been committed into a
// packed block is sealed, framed, uploaded via PutBlock, and atomically
// committed (record + locators + synced markers in one transaction, see
// DefaultCommitBlock). A crash between PutBlock and the commit leaves an
// orphan block object (reclaimed by GC) and re-carves the chunks into a new
// block — never losing or double-committing them.
//
// Return contract — see block.Flusher godoc for the full state
// machine and caller-retry guidance. In brief:
//   - Finalized=true, err=nil: durable on the configured remote.
//   - Finalized=false, err=nil: SOFT condition (no remote configured,
//     remote unhealthy, or the carve substrate is not wired). Callers
//     MUST NOT tight-loop retry (#670): surface the soft-fail to the
//     protocol adapter and let the client drive the next attempt on its
//     own schedule.
//   - err != nil: hard failure, do not retry until addressed.
//
// The carve drain serializes on carveMu against the background carve
// dispatcher, so an explicit Flush may block for the duration of an
// in-flight block build + PutBlock — bounded by one block (~16 MiB).
func (m *Syncer) Flush(ctx context.Context, payloadID string) (*block.FlushResult, error) {
	if err := m.checkReady(ctx); err != nil {
		return nil, err
	}

	// 1. Per-payload metadata quiesce: persist any FileChunk metadata
	//    that the local store has queued (queueFileChunkUpdate during
	//    rollup commit) so reads and restart-recovery see the
	//    authoritative manifest for this payloadID.
	m.local.SyncFileChunksForFile(ctx, payloadID)

	// 2. Local-only or remote-unhealthy: early-exit with Finalized=false.
	if m.remoteStore == nil || !m.IsRemoteHealthy() {
		return &block.FlushResult{Finalized: false}, nil
	}

	// A remote without the carve substrate (partial test fixture, or a local
	// backend without the log-blob substrate) cannot make anything durable:
	// report the soft condition instead of claiming durability.
	if !m.carveActive.Load() {
		return &block.FlushResult{Finalized: false}, nil
	}

	// 3. Drain the carve set (#1414): pack every pending chunk into block
	//    objects and commit them. Hashes added concurrently surface on the
	//    next Flush (snapshot-at-claim semantics).
	if err := m.carveFlush(ctx, true); err != nil {
		return nil, err
	}
	return &block.FlushResult{Finalized: true}, nil
}

// dataplaneMetrics returns the engine's data-plane metrics sink, or nil when
// the syncer is detached from a Store or no recorder was injected. Call sites
// must guard the result: it is a plain interface, not a nil-safe *Metrics.
func (m *Syncer) dataplaneMetrics() DataplaneMetrics {
	if m.bs == nil {
		return nil
	}
	if p := m.bs.metrics.Load(); p != nil {
		return *p
	}
	return nil
}

// PendingCount returns the number of CAS chunks present locally but not yet
// committed into a remote block — the live pending-carve backlog. Sourced
// from the addPendingHash/carve set, which is the actual upload path (unlike
// the vestigial SyncQueue).
func (m *Syncer) PendingCount() int {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	return len(m.pendingCarveHashes)
}

// SyncCounts returns lifetime (completed, failed) sync counts: chunks that
// reached remote and failed carve upload attempts.
func (m *Syncer) SyncCounts() (completed, failed int) {
	return int(m.completedSyncs.Load()), int(m.failedSyncs.Load())
}

// DrainAllUploads performs an immediate synchronous upload of every local
// block to remote, bypassing the UploadDelay. Returns nil when every block
// reached remote, ctx.Err() on cancellation, or an aggregated error naming
// the blocks that failed to upload.
//
// Exposed via the REST API for the benchmark runner to call between test
// phases, and used by Close() to ensure no blocks are left stranded in the
// local store at shutdown.
func (m *Syncer) DrainAllUploads(ctx context.Context) error {
	if err := m.SyncNow(ctx); err != nil {
		return err
	}
	return ctx.Err()
}

// GetFileSize returns the total size of a file from the remote store.
//
// Chunks live inside packed block objects, so the size is resolved via
// FileChunk metadata: enumerate every chunk belonging to payloadID, find the
// highest-offset remote-synced chunk, and compute
// size = chunkOffset + chunk.DataSize. DataSize is stamped at rollup time,
// so no extra S3 round-trip is needed.
//
// The carve path records synced markers via DefaultCommitBlock but never
// transitions FileChunk.State to BlockStateRemote (the row state remains
// Pending/Syncing for the life of the payload). The authoritative per-hash
// sync signal is therefore SyncedHashStore — not FileChunk.State. Each
// candidate row is included only if
// syncedHashStore.IsSynced(fb.Hash) returns true. If the SyncedHashStore
// is not wired (test fixtures), no chunks count as remote-mirrored and
// the function returns 0 — matching the pre-Phase-18 semantics where
// State==Remote was never set in that configuration either.
func (m *Syncer) GetFileSize(ctx context.Context, payloadID string) (uint64, error) {
	if err := m.checkReady(ctx); err != nil {
		return 0, err
	}

	if m.remoteStore == nil {
		logger.Debug("syncer: skipping GetFileSize, no remote store")
		return 0, nil
	}

	// Health gate: fail fast when remote is unreachable
	if !m.IsRemoteHealthy() {
		return 0, m.remoteUnavailableError()
	}

	blocks, err := m.fileChunkStore.ListFileChunks(ctx, payloadID)
	if err != nil {
		return 0, fmt.Errorf("list file blocks for %s: %w", payloadID, err)
	}
	if len(blocks) == 0 {
		return 0, nil
	}

	m.mu.RLock()
	hashStore := m.syncedHashStore
	m.mu.RUnlock()
	if hashStore == nil {
		// No mirror-state oracle wired — cannot prove any chunk is
		// remote-resident. Match the pre-fix behavior where State==Remote
		// was never observed without a SyncedHashStore.
		return 0, nil
	}

	// ListFileChunks returns blocks ordered by absolute chunk offset.
	// Walk from the end to find the highest-offset remote-mirrored chunk.
	// the trailing ID component is the chunk's absolute byte
	// Offset (FastCDC), not a synthetic blockIdx — do NOT multiply by
	// BlockSize.
	prefix := payloadID + "/"
	for i := len(blocks) - 1; i >= 0; i-- {
		fb := blocks[i]
		if fb == nil || fb.Hash.IsZero() {
			continue
		}
		if len(fb.ID) <= len(prefix) || fb.ID[:len(prefix)] != prefix {
			continue
		}
		synced, err := hashStore.IsSynced(ctx, fb.Hash)
		if err != nil {
			return 0, fmt.Errorf("is synced %s: %w", fb.Hash, err)
		}
		if !synced {
			continue
		}
		chunkOffset, ok := block.ParseChunkOffset(fb.ID)
		if !ok {
			continue
		}
		return chunkOffset + uint64(fb.DataSize), nil
	}
	return 0, nil
}

// Exists checks if any blocks exist for a file in the remote store.
//
// file existence is gated on SyncedHashStore — a chunk is
// considered remote-resident iff syncedHashStore.IsSynced(fb.Hash)
// returns true. The carve path does not transition FileChunk.State to
// BlockStateRemote, so the legacy State filter is no longer
// authoritative. If no SyncedHashStore is wired (test fixtures)
// Exists returns false — matching the pre-fix behavior under the same
// configuration.
func (m *Syncer) Exists(ctx context.Context, payloadID string) (bool, error) {
	if err := m.checkReady(ctx); err != nil {
		return false, err
	}
	if m.remoteStore == nil {
		logger.Debug("syncer: skipping Exists, no remote store")
		return false, nil
	}

	// Health gate: fail fast when remote is unreachable
	if !m.IsRemoteHealthy() {
		return false, m.remoteUnavailableError()
	}

	blocks, err := m.fileChunkStore.ListFileChunks(ctx, payloadID)
	if err != nil {
		return false, fmt.Errorf("list file blocks for %s: %w", payloadID, err)
	}

	m.mu.RLock()
	hashStore := m.syncedHashStore
	m.mu.RUnlock()
	if hashStore == nil {
		return false, nil
	}

	for _, fb := range blocks {
		if fb == nil || fb.Hash.IsZero() {
			continue
		}
		synced, err := hashStore.IsSynced(ctx, fb.Hash)
		if err != nil {
			return false, fmt.Errorf("is synced %s: %w", fb.Hash, err)
		}
		if synced {
			return true, nil
		}
	}
	return false, nil
}

// Truncate removes blocks beyond the new size from the remote store.
//
// Post-Phase-17 the engine is CAS-keyed: there is no per-file remote key
// prefix to enumerate. Truncate's metadata-side RefCount decrement runs
// inside engine.Truncate (which prunes FileAttr.Blocks and decrements per
// dropped hash); orphan CAS objects are reclaimed by the GC sweep. This
// method therefore becomes a no-op at the remote-side after
// kept as a stable seam for callers (engine.Truncate invokes it
// unconditionally) and so the legacy prefix-scan pattern is unambiguously
// gone.
func (m *Syncer) Truncate(ctx context.Context, payloadID string, newSize uint64) error {
	if err := m.checkReady(ctx); err != nil {
		return err
	}
	if m.remoteStore == nil {
		logger.Debug("syncer: skipping Truncate, no remote store")
		return nil
	}
	// Health gate retained for symmetry with the pre-CAS contract; the
	// remote-side cleanup itself is delegated to GC + refcount drops.
	if !m.IsRemoteHealthy() {
		logger.Warn("Truncate: skipping remote cleanup, remote store unhealthy",
			"payloadID", payloadID, "newSize", newSize)
		return nil
	}
	return nil
}

// Delete removes all blocks for a file from the remote store.
//
// Post-Phase-17 the engine is CAS-keyed: file deletion routes through the
// refcount path (engine.Delete decrements RefCount per ChunkRef hash and
// orphan CAS objects are reclaimed by GC). The legacy per-file prefix
// sweep is gone — Delete now records the deletion intent and lets
// the refcount + GC mechanism do the work.
func (m *Syncer) Delete(ctx context.Context, payloadID string) error {
	if err := m.checkReady(ctx); err != nil {
		return err
	}

	if m.remoteStore == nil {
		logger.Debug("syncer: skipping Delete, no remote store")
		return nil
	}
	if !m.IsRemoteHealthy() {
		logger.Warn("Delete: skipping remote cleanup, remote store unhealthy",
			"payloadID", payloadID)
		return nil
	}
	return nil
}

// Start begins background upload processing and periodic uploader.
// Must be called after New() to enable async uploads.
// When remoteStore is nil (local-only mode), the periodic syncer is skipped.
func (m *Syncer) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.queue != nil {
		m.queue.Start(ctx)
	}

	if m.remoteStore == nil {
		logger.Info("Syncer started in local-only mode (no remote store)")
		return
	}

	// one-shot janitor pass before the periodic uploader
	// starts. Requeues Syncing rows abandoned by a previous instance.
	// Failure here is logged at WARN — a bad metadata read should not
	// prevent the syncer from running its periodic loop.
	if err := m.recoverStaleSyncing(ctx); err != nil {
		logger.Warn("Syncer janitor: recoverStaleSyncing failed", "error", err)
	}

	// Seed the pending-upload set from disk: after a restart the volatile
	// set is empty, so chunks written-but-not-synced before shutdown would
	// otherwise never upload. This is the full walk, run once at startup.
	if n, err := m.seedPendingFromDisk(ctx); err != nil {
		logger.Warn("Syncer: seedPendingFromDisk failed; periodic reconcile will retry", "error", err)
	} else if n > 0 {
		logger.Info("Syncer: seeded pending uploads from disk", "count", n)
	}

	m.startHealthMonitor(ctx)
	m.startPeriodicUploader(ctx)
}

// startHealthMonitor creates and starts the health monitor for the remote store.
// Must be called with m.mu held.
func (m *Syncer) startHealthMonitor(ctx context.Context) {
	m.healthMonitor = NewHealthMonitor(m.remoteStore.HealthCheck, m.config)
	// Wrap the user's callback to also reset the offline-read WARN flag
	// on each healthy->unhealthy transition.
	userCallback := m.onHealthChanged
	m.healthMonitor.SetTransitionCallback(func(healthy bool) {
		if !healthy {
			m.firstOfflineRead.Store(false)
		}
		if userCallback != nil {
			userCallback(healthy)
		}
	})
	m.healthMonitor.Start(ctx)
}

// startPeriodicUploader launches the carve dispatcher and the maintenance
// loop, if not already running. Must be called with m.mu held.
func (m *Syncer) startPeriodicUploader(ctx context.Context) {
	if m.periodicStarted {
		return
	}
	// Manual-sync mode: durability is driven solely by explicit Flush, so the
	// background carver must not run. This makes Flush the single,
	// deterministic durability driver — required to observe snapshot-bounded /
	// crash-replay semantics that a concurrent carver would otherwise race.
	if m.config.ManualSync {
		m.periodicStarted = true
		return
	}
	m.periodicStarted = true

	interval := m.config.UploadInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	// The carve dispatcher packs log-blob chunks into block objects (#1414)
	// in steady state; the maintenance loop handles the slow housekeeping
	// (FileChunk metadata flush, drift reconcile) the dispatcher does not.
	go m.maintenanceLoop(ctx, interval)
	go m.carveDispatcher(ctx)

	// Adaptive mode (ParallelUploads unset): launch the goodput controller that
	// resizes the upload window to saturate the uplink (#1407). Pinned
	// --parallel-uploads leaves uploadController nil and keeps the fixed window;
	// publish it once so the gauge reflects it instead of reading 0.
	if m.uploadController != nil {
		go m.runUploadController(ctx, uploadControlInterval)
	} else if mx := m.dataplaneMetrics(); mx != nil && m.uploadLimiter != nil {
		mx.SetUploadWindow(m.uploadLimiter.Limit())
	}
}

// runUploadController is the adaptive upload-concurrency control loop (#1407).
// Every interval it turns the bytes/error accumulated by carveAndCommitBlock
// into a goodput sample, feeds the goodputController, and applies the returned
// window to the shared uploadLimiter. Runs only in adaptive mode (controller
// non-nil). Idle intervals (no bytes, nothing in flight, no error) are skipped
// so a write pause is not misread as a goodput collapse.
func (m *Syncer) runUploadController(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Publish the starting window so the gauge is populated before the first
	// adjustment.
	if mx := m.dataplaneMetrics(); mx != nil {
		mx.SetUploadWindow(m.uploadLimiter.Limit())
	}
	seconds := interval.Seconds()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.adaptiveUploadTick(seconds)
		}
	}
}

// adaptiveUploadTick converts one control interval's accumulated bytes and
// error flag into a goodput sample, feeds the controller, and applies the
// resulting window to the upload limiter. Extracted from the goroutine loop so
// the bytes→goodput→window glue is unit-testable without a clock. intervalSec
// is the control interval in seconds.
func (m *Syncer) adaptiveUploadTick(intervalSec float64) {
	bytes := m.uploadedBytesWindow.Swap(0)
	sawErr := m.uploadErrWindow.Swap(0) > 0
	// Peak in-flight over the interval distinguishes window-limited from
	// app-limited: uploads that filled the window mean goodput reflects the
	// window; otherwise the upstream carve pipeline was the constraint (see
	// goodputController.observe).
	peak := m.uploadLimiter.TakePeak()
	windowLimited := peak >= m.uploadLimiter.Limit()

	if bytes == 0 && peak == 0 && !sawErr {
		// Idle interval: no control decision, but publish an honest zero so the
		// goodput gauge does not freeze at the last active sample.
		if mx := m.dataplaneMetrics(); mx != nil {
			mx.SetUploadGoodput(0)
		}
		return
	}

	goodput := float64(bytes) / intervalSec
	window := m.uploadController.observe(goodput, windowLimited, sawErr)
	m.uploadLimiter.SetLimit(window)
	if mx := m.dataplaneMetrics(); mx != nil {
		mx.SetUploadWindow(window)
		mx.SetUploadGoodput(goodput)
	}
}

// maintenanceLoop runs the slow periodic housekeeping that the steady-state
// carve dispatcher does not: it persists queued FileChunk metadata and
// periodically re-seeds the pending-carve set from disk (drift reconcile) so
// chunks that predate the dispatcher — or whose carve attempt was dropped —
// are retried. It never uploads — that is the carve dispatcher's job.
func (m *Syncer) maintenanceLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	reconcileEvery := int((10 * time.Minute) / interval)
	if reconcileEvery < 1 {
		reconcileEvery = 1
	}
	tick := 0

	for {
		select {
		case <-ticker.C:
			if !m.canProcess(ctx) {
				return
			}
			tick++
			// Persist queued FileChunk metadata so reads/restart-recovery see
			// the authoritative manifest for recently rolled-up chunks.
			m.local.SyncFileChunks(ctx)
			if tick%reconcileEvery == 0 {
				if _, err := m.seedPendingFromDisk(ctx); err != nil {
					logger.Warn("Maintenance loop: drift reconcile failed", "error", err)
				}
			}
		case <-m.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// SyncNow triggers an immediate carve drain of every locally stored chunk
// that has not yet been committed into a remote block. Blocks until the pass
// completes or the context is cancelled. Returns nil on full success,
// ctx.Err() on cancellation, or a wrapped error from the carve pass. Callers
// such as the REST /drain-uploads endpoint and Close() rely on this signal.
//
// Serializes against the background carve dispatcher via carveMu (inside
// carveFlush), so the explicit drain never packs the same chunk twice.
func (m *Syncer) SyncNow(ctx context.Context) error {
	if m.remoteStore == nil {
		return nil
	}

	// Flush queued FileChunk metadata to the store so the drain pass
	// picks up recently rolled-up chunks.
	m.local.SyncFileChunks(ctx)

	if !m.carveActive.Load() {
		// A remote without the carve substrate cannot drain anything. Fail
		// honestly when chunks are pending rather than claiming durability.
		if m.PendingCount() > 0 {
			return errors.New("syncer: carve substrate not wired — pending chunks cannot reach remote")
		}
		return nil
	}

	return m.carveFlush(ctx, true)
}

// recoverStaleSyncing requeues blocks left in Syncing by a previous
// run (e.g., process killed mid-upload). Any Syncing row whose
// LastSyncAttemptAt is older than cfg.ClaimTimeout is flipped back
// to Pending with LastSyncAttemptAt cleared. CAS idempotency makes
// the re-upload safe even if the original upload eventually
// completes — both writes target byte-identical bytes at
// byte-identical keys.
//
// Backends that opt in to syncingEnumerator return precise candidates
// others degrade to a no-op.
func (m *Syncer) recoverStaleSyncing(ctx context.Context) error {
	if m.fileChunkStore == nil {
		return nil
	}
	enum, ok := m.fileChunkStore.(syncingEnumerator)
	if !ok {
		return nil
	}
	candidates, err := enum.EnumerateSyncingBlocks(ctx)
	if err != nil {
		return fmt.Errorf("enumerate syncing blocks: %w", err)
	}
	cutoff := time.Now().Add(-m.config.ClaimTimeout)
	requeued := 0
	failed := 0
	var firstErr error
	for _, fb := range candidates {
		if fb.State != block.BlockStateSyncing {
			continue
		}
		if !fb.LastSyncAttemptAt.IsZero() && fb.LastSyncAttemptAt.After(cutoff) {
			continue
		}
		fb.State = block.BlockStatePending
		fb.LastSyncAttemptAt = time.Time{}
		if err := m.fileChunkStore.Put(ctx, fb); err != nil {
			// elevate per-row failure to ERROR and track
			// counts so a fully-broken metadata path produces a non-nil
			// return error visible to the caller (Start logs it at WARN).
			logger.Error("janitor: requeue failed", "blockID", fb.ID, "error", err)
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		requeued++
	}
	if requeued > 0 {
		logger.Info("Syncer janitor requeued stale Syncing rows",
			"count", requeued, "claim_timeout", m.config.ClaimTimeout)
	}
	if failed > 0 {
		return fmt.Errorf("janitor: %d of %d candidate rows failed to requeue (first error: %w)",
			failed, failed+requeued, firstErr)
	}
	return nil
}

// syncingEnumerator is an optional capability a FileChunkStore may
// implement so the syncer's restart-recovery janitor can find stale
// Syncing rows without a full table scan.
type syncingEnumerator interface {
	EnumerateSyncingBlocks(ctx context.Context) ([]*block.FileChunk, error)
}

// Close shuts down the syncer and waits for pending uploads.
func (m *Syncer) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()

	close(m.stopCh)

	if m.healthMonitor != nil {
		m.healthMonitor.Stop()
	}

	// Wait for in-flight uploads and flushes to complete before closing.
	// This prevents "store is closed" races when the remote store is closed
	// immediately after the syncer.
	ctx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
	defer cancel()
	_ = m.DrainAllUploads(ctx)

	// Stop transfer queue with graceful shutdown timeout
	if m.queue != nil {
		m.queue.Stop(defaultShutdownTimeout)
	}

	return nil
}

// HealthCheck verifies the remote store is accessible.
// Returns nil (healthy) when remoteStore is nil -- local-only mode is valid.
func (m *Syncer) HealthCheck(ctx context.Context) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed {
		return ErrClosed
	}

	if m.remoteStore == nil {
		return nil // Local-only mode is healthy
	}

	return m.remoteStore.HealthCheck(ctx)
}

// SetRemoteStore transitions the syncer from local-only mode to remote-backed mode.
// This is a one-shot operation -- calling it again returns an error.
// It sets the remoteStore, enables local store eviction, and starts the periodic syncer.
//
// It does NOT seed the pending-upload set from disk: chunks written while the
// syncer was local-only are picked up by the next periodic drift reconcile
// (seedPendingFromDisk), not immediately. Not currently wired into any
// production control-plane path; Start() is the seeded entry point.
func (m *Syncer) SetRemoteStore(ctx context.Context, remoteStore remote.RemoteStore) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return ErrClosed
	}
	if m.remoteStore != nil {
		return errors.New("remote store already set")
	}
	if remoteStore == nil {
		return errors.New("remoteStore must not be nil")
	}

	m.remoteStore = remoteStore
	m.hasRemote.Store(true)
	m.recomputeCarveActive()
	m.local.SetEvictionEnabled(true)

	m.startHealthMonitor(ctx)
	m.startPeriodicUploader(ctx)

	logger.Info("Remote store attached, periodic syncer started")
	return nil
}
