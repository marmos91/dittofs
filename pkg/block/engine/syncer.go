package engine

import (
	"context"
	"fmt"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/block/syncer"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// defaultShutdownTimeout is the maximum time to wait for the transfer queue
// to finish processing during graceful shutdown.
const defaultShutdownTimeout = 30 * time.Second

// ponytail: one struct and one m.mu span fetch-dedup, readahead, health and
// carve wiring, because every one of those fields is read on the lock-ordering
// path whose failure mode is silent zeros. Splitting the carve wiring into its
// own collaborator with its own lock buys legibility and costs a second lock
// order to get right; do it only once a hardware rig can prove the split
// preserves the fetch/carve/close ordering.
// RemoteSync handles async local-to-remote transfers with eager block carving,
// parallel download, prefetch, in-flight dedup, and content-addressed dedup.
type RemoteSync struct {
	local       local.LocalStore
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
	// SetSyncedHashStore.
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
func NewRemoteSync(local local.LocalStore, remoteStore remote.RemoteStore, fileChunkStore block.EngineFileChunkStore, config RemoteSyncConfig) *RemoteSync {
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
func (m *RemoteSync) GetFileSize(ctx context.Context, payloadID string) (uint64, error) {
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
	for i := len(blocks) - 1; i >= 0; i-- {
		fb := blocks[i]
		if fb == nil || fb.Hash.IsZero() {
			continue
		}
		chunkOffset, ok := block.ChunkOffsetFor(fb.ID, payloadID)
		if !ok {
			continue
		}
		synced, err := hashStore.IsSynced(ctx, fb.Hash)
		if err != nil {
			return 0, fmt.Errorf("is synced %s: %w", fb.Hash, err)
		}
		if !synced {
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
func (m *RemoteSync) Exists(ctx context.Context, payloadID string) (bool, error) {
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

// Truncate is a no-op on the remote side; the CAS objects a truncate orphans
// are reclaimed by the GC sweep.
//
// Post-Phase-17 the engine is CAS-keyed: there is no per-file remote key
// prefix to enumerate. Truncate's metadata-side RefCount decrement runs
// inside engine.Truncate (which prunes FileAttr.Blocks and decrements per
// dropped hash), which is what makes a hash sweepable. This method is kept as
// a stable seam for callers — engine.Truncate invokes it unconditionally — and
// so the absence of the legacy prefix scan is explicit rather than inferred
// from a missing call.
func (m *RemoteSync) Truncate(ctx context.Context, payloadID string, newSize uint64) error {
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

// Delete is a no-op on the remote side; engine.Delete drives the deletion.
//
// Post-Phase-17 the engine is CAS-keyed: file deletion routes through the
// refcount path — engine.Delete decrements RefCount per ChunkRef hash, and the
// GC sweep reclaims the CAS objects that leaves orphaned. Nothing is removed or
// recorded here. The legacy per-file prefix sweep is gone, and this method is
// kept as a stable seam for the call in engine.Delete.
func (m *RemoteSync) Delete(ctx context.Context, payloadID string) error {
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
func (m *RemoteSync) Start(ctx context.Context) {
	// The health monitor's eager probe is a network round trip, so it runs
	// after m.mu is released: every read and flush path takes that lock, and
	// holding it for a remote timeout stalls them all. Start still returns only
	// once the probe has settled, so callers may read the health state.
	if hm := m.startLocked(ctx); hm != nil {
		hm.Start(ctx)
	}
}

// startLocked performs the locked half of Start and returns the health monitor
// still to be started, or nil in local-only mode.
func (m *RemoteSync) startLocked(ctx context.Context) *HealthMonitor {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.queue != nil {
		m.queue.Start(ctx)
	}

	if m.remoteStore == nil {
		logger.Info("RemoteSync started in local-only mode (no remote store)")
		return nil
	}

	// one-shot janitor pass before the periodic uploader
	// starts. Requeues Syncing rows abandoned by a previous instance.
	// Failure here is logged at WARN — a bad metadata read should not
	// prevent the syncer from running its periodic loop.
	if err := m.recoverStaleSyncing(ctx); err != nil {
		logger.Warn("RemoteSync janitor: recoverStaleSyncing failed", "error", err)
	}

	// No pending-set to seed from disk: the journal owns the unsynced state
	// (its recovered interval index re-marks every not-yet-carved record dirty
	// on Open), so the carve dispatcher re-drains them without a reconcile walk.

	hm := m.newHealthMonitorLocked()
	m.startPeriodicUploader(ctx)
	return hm
}

// startPeriodicUploader launches the carve dispatcher and the maintenance
// loop, if not already running. Must be called with m.mu held.
func (m *RemoteSync) startPeriodicUploader(ctx context.Context) {
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

	// Carve collaborators are wired by recomputeCarveActive once all deps are
	// present; launch the dispatcher that periodically packs the journal's dirty
	// ranges into remote blocks.
	m.bgWG.Add(1)
	go func() {
		defer m.bgWG.Done()
		m.carveDispatcher(ctx)
	}()

	// Adaptive mode (ParallelUploads unset): launch the goodput controller that
	// resizes the upload window to saturate the uplink. Pinned
	// --parallel-uploads leaves uploadController nil and keeps the fixed window;
	// publish it once so the gauge reflects it instead of reading 0.
	if m.uploadController != nil {
		m.bgWG.Add(1)
		go func() {
			defer m.bgWG.Done()
			m.runUploadController(ctx, uploadControlInterval)
		}()
	} else if mx := m.dataplaneMetrics(); mx != nil && m.uploadLimiter != nil {
		mx.SetUploadWindow(m.uploadLimiter.Limit())
	}
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
func (m *RemoteSync) recoverStaleSyncing(ctx context.Context) error {
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
		logger.Info("RemoteSync janitor requeued stale Syncing rows",
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
func (m *RemoteSync) Close() error {
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

	// Join the background loops LAST. They observe stopCh, but a carve pass can
	// be parked in an upload that only the drain and queue stop above release,
	// so joining earlier would deadlock. Joining at all is what stops Close from
	// returning while a pass is still reading the local store or writing the
	// remote — the engine closes both the moment Close returns.
	if !waitBounded(&m.bgWG, defaultShutdownTimeout) {
		logger.Warn("RemoteSync background loops did not exit before shutdown timeout")
	}

	return nil
}

// waitBounded waits for wg, reporting whether it drained before timeout. The
// wait is bounded because a background loop can be parked in a remote call with
// its own retry budget: shutdown logs and proceeds rather than wedging on it.
func waitBounded(wg *gosync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
