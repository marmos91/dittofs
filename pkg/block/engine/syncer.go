package engine

import (
	"context"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/block/syncer"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ponytail: one struct and one m.mu span fetch-dedup, readahead, health and
// carve wiring, because every one of those fields is read on the lock-ordering
// path whose failure mode is silent zeros. Splitting the carve wiring into its
// own collaborator with its own lock buys legibility and costs a second lock
// order to get right; do it only once a hardware rig can prove the split
// preserves the fetch/carve/close ordering.
// RemoteSync handles async local-to-remote transfers with eager block carving,
// parallel download, prefetch, in-flight dedup, and content-addressed dedup.
type RemoteSync struct {
	local       journal.LocalStore
	remoteStore remote.RemoteStore
	// hasRemote mirrors "remoteStore != nil" as an atomic so hot-path gating
	// (the carveActive recompute and the readahead scheduler) can read it
	// without taking m.mu, avoiding a data race with Start.
	hasRemote atomic.Bool
	// fileChunkStore is the per-file chunk manifest, and the syncer needs the
	// wide EngineFileChunkStore surface rather than a narrower one because it
	// reads the manifest three different ways: GetFileChunk to resolve a single
	// row, ListFileChunks to enumerate a payload for GetFileSize/Exists and the
	// window walks, and the offset-indexed lookups where a backend offers them.
	fileChunkStore block.EngineFileChunkStore // Required: enables content-addressed deduplication

	// syncedHashStore persists per-CAS-hash local→remote sync state. The
	// restart/drift reseed consumes local.ListUnsynced (which itself filters
	// via SyncedHashStore.IsSynced); the carver records synced markers +
	// block locators atomically via DefaultCommitBlock. May be nil in unit
	// tests / local-only fixtures; production callers wire a real store via
	// SetSyncedHashStore. Guarded by m.mu.
	syncedHashStore metadata.SyncedHashStore

	// metrics points at the owning Store's data-plane metrics cell, not at a
	// copy of its value: SetMetrics back-fills that cell on already-serving
	// shares, so a value captured at construction time would stay nil for the
	// lifetime of the syncer. Nil in pre-wiring tests; callers must nil-check.
	metrics *atomic.Pointer[DataplaneMetrics]

	config RemoteSyncConfig

	queue *SyncQueue // Transfer queue for non-blocking operations

	inFlight   map[string]*fetchResult // In-flight download dedup (store key -> broadcast)
	inFlightMu gosync.Mutex

	// readahead holds per-payload sequential-access frontier state so remote
	// prefetch ramps on sequential runs and backs off on random access (see
	// readahead.go). A gosync.Map + atomic raState fields keep the read hot path
	// lock-free: planWindow previously took a single global mutex on EVERY read
	// (whenever a remote is configured), so a concurrent 4k random-read fleet on
	// one payload serialized there. Readahead state is a disposable heuristic, so
	// the atomic load-decide-store races are benign (a stale frontier only
	// mis-sizes prefetch, never affects correctness).
	readahead        gosync.Map   // payloadID(string) -> *raState
	readaheadN       atomic.Int64 // approximate entry count, for bounding
	readaheadPruning atomic.Bool  // single-pruner guard for the bound

	stopCh chan struct{} // Signals periodic uploader to stop
	// bgWG counts the long-lived loops started by startPeriodicUploader. Close
	// joins them so it does not return while one is still calling into the
	// local or remote store, which the engine closes as soon as Close returns.
	// The join is bounded: a loop parked in a remote call past the shutdown
	// timeout is logged and left behind rather than allowed to wedge shutdown.
	bgWG   gosync.WaitGroup
	closed bool
	mu     gosync.RWMutex

	periodicStarted bool // true once the carve dispatcher goroutine is launched

	healthMonitor   *HealthMonitor           // Monitors remote store health (nil when no remote)
	onHealthChanged healthTransitionCallback // Callback invoked on health state transitions

	firstOfflineRead    atomic.Bool  // Tracks if WARN was already logged since last healthy->unhealthy transition
	offlineReadsBlocked atomic.Int64 // Count of read operations blocked by remote unavailability

	// completedSyncs / failedSyncs are lifetime counters of CAS chunks that
	// reached remote (committed inside a packed block) and of failed carve
	// upload attempts. They source the truthful CompletedSyncs / FailedSyncs
	// fields in block stats; the legacy SyncQueue has no production upload
	// callers, so its counters always read zero.
	completedSyncs atomic.Int64
	failedSyncs    atomic.Int64

	// uploadLimiter bounds concurrent block PUTs across the whole syncer: every
	// flush pass shares it, holding one slot per block from submit until that
	// block's CommitBlock returns. It is the only bound on upload concurrency,
	// so Limit() is the number the config declares and the peak the controller
	// samples. How many files carve at once is a separate fixed cap
	// (carveFanOut) that holds no upload slot of its own.
	// When ParallelUploads is pinned (> 0) its limit is fixed at that value.
	// When unset (adaptive mode) the uploadController resizes it every control
	// interval to track the goodput knee.
	uploadLimiter *syncer.DynamicSemaphore
	// uploadController is non-nil only in adaptive mode. It consumes one
	// (goodput, windowLimited, sawError) sample per control interval and returns
	// the next target window, applied to uploadLimiter by the control goroutine.
	uploadController *syncer.GoodputController
	// uploadedBytesWindow accumulates bytes successfully PutBlock'd since the
	// last control tick; uploadErrWindow counts block-upload errors in the same
	// span. The control goroutine swaps both to zero each tick to compute the
	// goodput sample and the error flag. Plain atomics — no lock needed.
	uploadedBytesWindow atomic.Int64
	uploadErrWindow     atomic.Int64

	// putSample tracks concurrent PutBlock calls directly, which is what the
	// controller samples. The upload window cannot stand in for it: a slot is
	// held across PutBlock AND the metadata commit that follows, so a slow
	// per-file commit fills the window after the uploads have finished and the
	// window's own peak then reports uplink saturation that is really commit
	// backpressure. This counts only the time inside PutBlock.
	//
	// The live count and the high-water mark share ONE word — peak in the high
	// 32 bits, in-flight in the low 32 — so that sampling them is a single
	// atomic step. Held as two atomics they could not be read together: a PUT
	// that had incremented the in-flight count but not yet raised the peak
	// would be folded into the baseline the sampler installed and then find
	// nothing left to raise, so the interval it overlapped reported a peak one
	// short. That loss only ever runs downward, and an under-reported peak is
	// what reads as app-limited — the misclassification this whole path exists
	// to remove.
	putSample atomic.Uint64

	// --- block carve path (object packing) ---

	// remoteBlockStore is the block-keyed remote (PutBlock) the carver uploads
	// packed blocks to. nil disables carve. Wired via SetRemoteBlockStore;
	// guarded by m.mu.
	remoteBlockStore remote.RemoteBlockStore
	// chunkSealer applies the per-chunk compression/encryption transform before
	// a chunk is framed into a block. Derived from remoteBlockStore (the same
	// decorated remote); nil means identity (raw) sealing. Guarded by m.mu.
	chunkSealer remote.ChunkSealer
	// blockCommitter atomically persists the block record + synced markers
	// (DefaultCommitBlock) — the per-share metadata store the carve BlockSink
	// commits through. nil disables carve. Guarded by m.mu.
	blockCommitter blockCommitter

	// carveActive mirrors "all carve deps wired AND a remote exists" as an
	// atomic so hot paths (Flush honesty check, the dispatcher early-out) can
	// read it without taking m.mu. Recomputed by the setters.
	carveActive atomic.Bool
}

// blockCommitter is the narrow consumer-side slice of metadata.Store the carver
// needs: transactional block-record commit (DefaultCommitBlock takes a
// Transactor+SyncedHashStore) and the synced-marker writes it performs. The
// production per-share metadata store satisfies both; defining it here keeps the
// engine off the wider metadata.Store surface.
type blockCommitter interface {
	metadata.Transactor
	metadata.SyncedHashStore
}

// UnsyncedBytes returns the on-disk size of local ranges not yet carved to the
// remote. It is the journal's own dirty-byte counter — the backpressure signal
// the eviction path consults: a non-zero value with a healthy remote means a
// stalled writer can make progress once the carve dispatcher drains.
func (m *RemoteSync) UnsyncedBytes() int64 {
	return m.local.UnsyncedBytes()
}

// NewRemoteSync creates a new RemoteSync. The fileChunkStore is required for content-addressed dedup.
func NewRemoteSync(local journal.LocalStore, remoteStore remote.RemoteStore, fileChunkStore block.EngineFileChunkStore, config RemoteSyncConfig) *RemoteSync {
	if fileChunkStore == nil {
		panic("fileChunkStore is required for RemoteSync")
	}
	if config.ParallelDownloads <= 0 {
		config.ParallelDownloads = DefaultParallelDownloads
	}
	if config.PrefetchBlocks <= 0 {
		config.PrefetchBlocks = DefaultPrefetchBlocks
	}
	if config.DemandFetchTimeout <= 0 {
		config.DemandFetchTimeout = DefaultDemandFetchTimeout
	}
	// — apply CAS-path defaults.
	if config.ClaimTimeout <= 0 {
		config.ClaimTimeout = 10 * time.Minute
	}

	// Upload concurrency: a pinned ParallelUploads > 0 fixes the window;
	// otherwise (the default) the carver auto-tunes between the adaptive
	// floor and ceiling. The limiter starts at the floor in adaptive mode and at
	// the pinned value otherwise; the control goroutine (adaptive only, launched
	// in Start) resizes it at runtime.
	var uploadController *syncer.GoodputController
	startWindow := config.ParallelUploads
	if config.ParallelUploads <= 0 {
		startWindow = AdaptiveUploadFloor
		uploadController = syncer.NewGoodputController(AdaptiveUploadFloor, AdaptiveUploadCeiling)
	}

	m := &RemoteSync{
		local:          local,
		remoteStore:    remoteStore,
		fileChunkStore: fileChunkStore,
		config:         config,
		inFlight:       make(map[string]*fetchResult),
		stopCh:         make(chan struct{}),

		uploadLimiter:    syncer.NewDynamicSemaphore(startWindow),
		uploadController: uploadController,
	}
	m.hasRemote.Store(remoteStore != nil)
	m.recomputeCarveActive()

	queueConfig := DefaultSyncQueueConfig()
	queueConfig.DownloadWorkers = config.ParallelDownloads
	m.queue = NewSyncQueue(m, queueConfig)

	return m
}

// Queue returns the transfer queue for stats inspection.
func (m *RemoteSync) Queue() *SyncQueue { return m.queue }

// SetSyncedHashStore wires the per-hash sync-state store the restart/drift
// reseed consults via local.ListUnsynced and the carver updates through
// DefaultCommitBlock. Idempotent. May be invoked after NewRemoteSync so the
// construction sequence does not need to thread a SyncedHashStore through
// the engine.NewRemoteSync signature.
func (m *RemoteSync) SetSyncedHashStore(s metadata.SyncedHashStore) {
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
func (m *RemoteSync) SetRemoteBlockStore(rbs remote.RemoteBlockStore) {
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
func (m *RemoteSync) recomputeCarveActive() {
	active := m.remoteBlockStore != nil &&
		m.blockCommitter != nil &&
		m.hasRemote.Load()
	m.carveActive.Store(active)
}

// wiring snapshots the four carve deps the setters above publish, under one
// acquisition of the lock they publish them with. Reading them a field at a
// time would let a caller build a flush closure from a torn mix of two
// wirings: SetRemoteBlockStore derives the sealer from the store and publishes
// both together, so a sealer from the old remote paired with the new remote's
// store is reachable the moment the two reads are separate. One acquisition
// also keeps the caller off the four-lock path the field-by-field alternative
// would need.
func (m *RemoteSync) wiring() (remote.RemoteBlockStore, remote.ChunkSealer, blockCommitter, metadata.SyncedHashStore) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.remoteBlockStore, m.chunkSealer, m.blockCommitter, m.syncedHashStore
}

// SetHealthCallback sets the callback invoked when the remote store health state changes.
// If the HealthMonitor is already running, the callback is forwarded to it immediately.
func (m *RemoteSync) SetHealthCallback(fn healthTransitionCallback) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onHealthChanged = fn
	if m.healthMonitor != nil {
		m.healthMonitor.SetTransitionCallback(fn)
	}
}

// checkReady returns nil if the syncer can process requests.
// Returns ctx.Err() if the context is cancelled, or ErrClosed if the RemoteSync is closed.
func (m *RemoteSync) checkReady(ctx context.Context) error {
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

// canProcess returns false if the RemoteSync is closed or context is cancelled.
func (m *RemoteSync) canProcess(ctx context.Context) bool {
	return m.checkReady(ctx) == nil
}
