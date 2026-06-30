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
	"golang.org/x/sync/errgroup"
	"lukechampine.com/blake3"
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

// Syncer handles async local-to-remote transfers with eager upload
// parallel download, prefetch, in-flight dedup, and content-addressed dedup.
type Syncer struct {
	local       local.LocalStore
	remoteStore remote.RemoteStore
	// hasRemote mirrors "remoteStore != nil" as an atomic so the dispatcher's
	// hot-path gating (dispatcherEnabled, called from addPendingHash under
	// pendingMu) can read it without taking m.mu — avoiding both a data race
	// with SetRemoteStore and a pendingMu→m.mu lock-ordering edge.
	hasRemote atomic.Bool
	// the syncer is one of the engine-internal
	// callers that still reaches into the wider EngineFileBlockStore
	// surface (GetFileBlock for dual-read resolve, ListFileBlocks for
	// GetFileSize/Exists). routes reads through
	// FileAttr.Blocks and lets us drop the wider interface.
	fileBlockStore block.EngineFileBlockStore // Required: enables content-addressed deduplication

	// syncedHashStore persists per-CAS-hash local→remote mirror state.
	// The mirror loop in Flush consumes ListUnsynced (which itself
	// filters via SyncedHashStore.IsSynced) and calls MarkSynced after
	// each successful remote.Put. May be nil in unit tests / local-only
	// fixtures; production callers wire a real store via
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

	stopCh chan struct{} // Signals periodic uploader to stop
	closed bool
	mu     gosync.RWMutex

	// wake nudges the upload dispatcher that work is ready, so it picks up a
	// freshly rolled-up chunk immediately instead of idling. Signalled
	// (non-blocking, coalescing) from addPendingHash on every freshly rolled-up
	// chunk, so upload overlaps the rollup of later chunks rather than starting
	// a full tick-interval after rollup finishes (#1407). Buffered length 1: a
	// burst of completions collapses to a single wakeup, after which the
	// dispatcher drains the whole ready queue continuously (#1432).
	wake chan struct{}

	periodicStarted bool // true once the dispatcher + maintenance goroutines are launched

	// gate serializes the explicit drain path (Flush / SyncNow / uploadBlock)
	// against the continuous upload dispatcher so the two never upload the same
	// CAS chunk concurrently (#1432). See uploadGate.
	gate *uploadGate

	healthMonitor   *HealthMonitor           // Monitors remote store health (nil when no remote)
	onHealthChanged healthTransitionCallback // Callback invoked on health state transitions

	firstOfflineRead    atomic.Bool  // Tracks if WARN was already logged since last healthy->unhealthy transition
	offlineReadsBlocked atomic.Int64 // Count of read operations blocked by remote unavailability

	// pendingHashes maps each CAS hash present locally but not yet mirrored
	// to remote to its on-disk byte size. Populated O(1) by addPendingHash
	// (fired from the onChunkComplete chokepoint) and drained by mirrorOnce
	// after each MarkSynced. This replaces the per-tick full directory walk
	// of the CAS tree: the steady-state mirror loop now consumes this set
	// instead of rediscovering unsynced chunks via ListUnsynced. A startup
	// reconciliation (seedPendingFromDisk) re-seeds it after a restart,
	// since the set is volatile and orphaned chunks written-but-not-synced
	// before a crash would otherwise be missed.
	//
	// The per-hash size feeds unsyncedBytes, the backpressure signal the
	// local store consults to decide whether to keep stalling a writer.
	pendingMu     gosync.Mutex
	pendingHashes map[block.ContentHash]int64

	// readyQ is the FIFO of pending hashes ready for the dispatcher to upload,
	// and inflight is the set currently being uploaded. Both are guarded by
	// pendingMu and used only by the continuous dispatcher (#1432); the explicit
	// mirrorOnce path reads pendingHashes directly. Every pending hash is in
	// exactly one of {readyQ, inflight, failed-awaiting-requeue}; the
	// maintenance loop's requeueOrphans moves failed hashes back into readyQ.
	// claimReady validates on pop, so a stale/duplicate readyQ entry is skipped.
	readyQ   []block.ContentHash
	inflight map[block.ContentHash]struct{}

	// unsyncedBytes is the running total on-disk size of pendingHashes:
	// the number of cache bytes that cannot be evicted until they reach
	// remote. The local store reads it (via UnsyncedBytes) to decide
	// whether a backpressure stall can make progress. Charged once per
	// distinct hash (CAS dedup): re-adding a hash already pending does not
	// double-count, and a drained hash subtracts exactly what it added.
	unsyncedBytes atomic.Int64

	// completedSyncs / failedSyncs are lifetime counters of CAS chunks that
	// reached remote (Put + MarkSynced succeeded) and that failed a mirror
	// attempt (Put or MarkSynced errored). They source the truthful
	// CompletedSyncs / FailedSyncs fields in block stats; the legacy SyncQueue
	// has no production callers, so its counters always read zero (#1266).
	completedSyncs atomic.Int64
	failedSyncs    atomic.Int64

	// uploadLimiter bounds concurrent CAS-chunk uploads in mirrorOnce. When
	// --parallel-uploads is pinned (config.ParallelUploads > 0) its limit is
	// fixed at that value. When unset (adaptive mode), the uploadController
	// resizes it every control interval to track the goodput knee (#1407).
	uploadLimiter *dynamicSemaphore

	// uploadController is non-nil only in adaptive mode. It consumes one
	// (goodput, sawError) sample per control interval and returns the next
	// target window, which the controller goroutine applies to uploadLimiter.
	uploadController *goodputController

	// uploadedBytesWindow accumulates bytes successfully Put to remote since
	// the last control tick; uploadErrWindow counts upload errors in the same
	// span. The adaptive controller goroutine swaps both to zero each tick to
	// compute goodput and the error flag. Plain atomics — no lock needed.
	uploadedBytesWindow atomic.Int64
	uploadErrWindow     atomic.Int64
}

// addPendingHash registers a newly-stored CAS hash (of the given on-disk
// byte size) for the next mirror pass. Fired from the onChunkComplete
// callback (engine.New) on every successful StoreChunk. Safe for concurrent
// use; O(1). Charges unsyncedBytes once per distinct hash — re-adding a hash
// already pending updates the recorded size but does not double-count.
func (m *Syncer) addPendingHash(h block.ContentHash, size int64) {
	m.pendingMu.Lock()
	// prev is the zero value (0) when the hash is new, so size-prev charges
	// the full size on first insert and only the delta on re-add — never
	// double-counting a hash already pending (CAS dedup). The counter update
	// stays INSIDE pendingMu so it is serialized against mirrorOnce's drain
	// (which deletes from the map and subtracts under the same lock): an
	// add that interleaved a concurrent drain otherwise leaked a phantom
	// positive byte count that no future drain would ever subtract.
	prev, alreadyPending := m.pendingHashes[h]
	m.pendingHashes[h] = size
	m.unsyncedBytes.Add(size - prev)
	// Queue a freshly-pending hash for the dispatcher. An already-pending hash
	// is left where it is (in readyQ, in flight, or awaiting requeue) — only its
	// recorded size is refreshed above. readyQ/dispatch state is meaningful only
	// when the dispatcher runs (a remote exists and not manual-sync); otherwise
	// the explicit mirrorOnce path drains pendingHashes directly, so skipping
	// the append avoids unbounded readyQ growth with no consumer.
	queued := false
	if !alreadyPending && m.dispatcherEnabled() {
		m.readyQ = append(m.readyQ, h)
		queued = true
	}
	m.pendingMu.Unlock()

	// Only a newly-inserted hash changes the queue depth; re-adding an
	// already-pending hash (size refresh) leaves the gauge unchanged, so skip
	// the extra lock/metric churn on that hot path (incl. seedPendingFromDisk).
	if !alreadyPending {
		m.publishQueueDepth()
	}

	// Nudge the dispatcher to mirror this chunk now instead of idling, so upload
	// pipelines with the rollup of later chunks (#1407). Non-blocking +
	// coalescing — see signalWake.
	if queued {
		m.signalWake()
	}
}

// ensureUploadLimiter lazily creates the shared upload limiter for Syncers
// built directly (test fixtures) rather than via NewSyncer, which always wires
// it. A fixture has no adaptive controller goroutine, so the limiter stays at a
// fixed window: the pinned ParallelUploads if set, else the adaptive ceiling so
// fixtures get full concurrency. Matches the existing nil-fixture guards
// (syncedHashStore, bs, metrics). Cheap and idempotent under m.mu.
func (m *Syncer) ensureUploadLimiter() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.uploadLimiter != nil {
		return
	}
	start := m.config.ParallelUploads
	if start <= 0 {
		start = AdaptiveUploadCeiling
	}
	m.uploadLimiter = newDynamicSemaphore(start)
}

// signalWake performs a non-blocking, coalescing send on the wake channel,
// nudging the upload dispatcher to pick up freshly-ready work immediately
// instead of idling. Because wake is buffered length 1, a burst of chunk
// completions collapses into a single wakeup, after which the dispatcher drains
// the whole ready queue continuously — bounding wakeups regardless of chunk
// arrival rate (#1407).
func (m *Syncer) signalWake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// markFetchedSynced records a chunk that was just downloaded from the remote
// store as already mirrored. The bytes are verbatim remote content, so the
// chunk is provably durable on remote, and we must treat it differently from a
// locally-written chunk:
//
//  1. Cancel the pending-upload entry that StoreChunk's onChunkComplete
//     callback registered for it, so the mirror loop does not waste a redundant
//     remote.Put re-uploading data that is already there. Without this, reading
//     an N-byte archive over a remote tier re-uploads N bytes back to remote
//     (read-amplification → write-amplification, #1362).
//  2. Mark it synced so eviction's IsSynced gate can reclaim it immediately
//     rather than waiting for a mirror pass; otherwise the first eviction on a
//     read-only workload finds zero synced candidates and stalls.
//
// Nil-safe for test fixtures with no SyncedHashStore wired.
func (m *Syncer) markFetchedSynced(ctx context.Context, h block.ContentHash) {
	if h.IsZero() {
		return
	}
	m.mu.RLock()
	hashStore := m.syncedHashStore
	m.mu.RUnlock()
	if hashStore != nil {
		// Fetched bytes came verbatim from the chunk's own standalone CAS
		// object, so record a standalone locator (PackID==""). PR3b note: a
		// chunk fetched out of a pack is re-stored locally and, when later
		// re-mirrored, re-packed — markFetchedSynced still records standalone.
		if err := hashStore.MarkSynced(ctx, h, block.ChunkLocator{}); err != nil {
			// Non-fatal: leave the chunk pending so the mirror loop re-uploads
			// and marks it synced on the next tick (remote.Put is idempotent).
			logger.Warn("markFetchedSynced: MarkSynced failed; chunk will be re-mirrored",
				"hash", h.String(), "error", err)
			return
		}
	}
	// Drop the pending-upload entry StoreChunk's callback just registered so
	// the mirror loop skips it. A concurrent mirrorOnce tick that already
	// snapshotted this hash may still re-upload it once — harmless and rare.
	m.pendingMu.Lock()
	if size, ok := m.pendingHashes[h]; ok {
		delete(m.pendingHashes, h)
		m.unsyncedBytes.Add(-size)
	}
	m.pendingMu.Unlock()
	m.publishQueueDepth()
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

// seedPendingFromDisk reconciles the in-memory pending set against the
// on-disk CAS state by walking every locally-present chunk that is not yet
// marked synced (ListUnsynced) and adding it to the set. This is the
// O(total-chunks) directory walk — but it runs ONCE at startup (the
// pending set is volatile, so chunks written-but-not-synced before a crash
// must be rediscovered) and periodically as a slow drift reconciler, NOT
// on every mirror tick. Returns the number of hashes seeded.
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

// NewSyncer creates a new Syncer. The fileBlockStore is required for content-addressed dedup.
func NewSyncer(local local.LocalStore, remoteStore remote.RemoteStore, fileBlockStore block.EngineFileBlockStore, config SyncerConfig) *Syncer {
	if fileBlockStore == nil {
		panic("fileBlockStore is required for Syncer")
	}
	// Upload concurrency: a pinned ParallelUploads > 0 fixes the window;
	// otherwise (the default) the syncer auto-tunes between the adaptive floor
	// and ceiling (#1407). The limiter starts at the floor in adaptive mode and
	// at the pinned value otherwise; the controller goroutine (adaptive only)
	// resizes it at runtime.
	adaptive := config.ParallelUploads <= 0
	uploadCeiling := AdaptiveUploadCeiling
	var uploadController *goodputController
	startWindow := config.ParallelUploads
	if adaptive {
		startWindow = AdaptiveUploadFloor
		uploadController = newGoodputController(AdaptiveUploadFloor, AdaptiveUploadCeiling)
	} else if config.ParallelUploads > uploadCeiling {
		// A pinned window above the adaptive ceiling is honored; the queue
		// worker pool must cover it.
		uploadCeiling = config.ParallelUploads
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

	m := &Syncer{
		local:            local,
		remoteStore:      remoteStore,
		fileBlockStore:   fileBlockStore,
		config:           config,
		inFlight:         make(map[string]*fetchResult),
		stopCh:           make(chan struct{}),
		wake:             make(chan struct{}, 1),
		pendingHashes:    make(map[block.ContentHash]int64),
		inflight:         make(map[block.ContentHash]struct{}),
		gate:             newUploadGate(),
		uploadLimiter:    newDynamicSemaphore(startWindow),
		uploadController: uploadController,
	}
	m.hasRemote.Store(remoteStore != nil)

	queueConfig := DefaultSyncQueueConfig()
	// In adaptive mode the queue pool must cover the ceiling the controller can
	// ramp to; pinned mode sizes it at the fixed window.
	queueConfig.Workers = uploadCeiling
	queueConfig.DownloadWorkers = config.ParallelDownloads
	m.queue = NewSyncQueue(m, queueConfig)

	return m
}

// Queue returns the transfer queue for stats inspection.
func (m *Syncer) Queue() *SyncQueue { return m.queue }

// SetSyncedHashStore wires the per-hash mirror-state store the mirror
// loop in Flush consults via local.ListUnsynced and updates via
// MarkSynced after each remote.Put. Idempotent. May be invoked after
// NewSyncer so the construction sequence does not need to thread a
// SyncedHashStore through the engine.NewSyncer signature.
func (m *Syncer) SetSyncedHashStore(s metadata.SyncedHashStore) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncedHashStore = s
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

// Flush quiesces a payload's local-side state and mirrors every locally
// stored CAS chunk that has not yet been marked synced to the remote
// store. Mirror loop ordering is Put-then-Mark: each hash's bytes are
// written to remote.Put first, and only on success does the
// SyncedHashStore.MarkSynced call fire. A crash between the two steps
// is safe because remote.Put is idempotent on (hash, identical bytes)
// per the unified Store contract, so the next Flush pass re-Puts
// the same hash and proceeds to MarkSynced.
//
// Return contract — see block.Flusher godoc for the full state
// machine and caller-retry guidance. In brief:
//   - Finalized=true, err=nil: durable on the configured remote.
//   - Finalized=false, err=nil: SOFT condition (no remote configured,
//     remote unhealthy, OR the upload gate is held by another explicit
//     drain or in-flight dispatcher uploads). Callers MUST NOT tight-loop
//     retry — see #670 below.
//   - err != nil: hard failure, do not retry until addressed.
//
// #670: callers driving NFS COMMIT or SMB Flush loops over this method
// must rate-limit retries on Finalized=false. The non-blocking gate
// acquisition below makes the EXPLICIT Flush caller soft-fail any
// attempt that races the continuous dispatcher, so a tight in-handler
// retry loop pegs the CPU without ever making progress and starves the
// dispatcher. Recommended pattern: surface the
// soft-fail to the protocol adapter and let the client drive the next
// attempt on its own schedule (e.g. NFSv3 reports the WRITE's
// "committed" enum as UNSTABLE rather than DATASYNC/FILESYNC so the
// client reissues COMMIT later; SMB Flush returns success after a
// bounded attempt) instead of spinning in-handler.
//
// #1432: the continuous dispatcher keeps the gate active for as long as
// uploads are in flight, so under a sustained streaming workload Flush may
// soft-fail (Finalized=false) until the pipeline momentarily idles. This is
// expected and does not affect crash-safety: chunks are already durable in the
// local log before Flush is ever called, and the dispatcher mirrors them to the
// remote in the background. Callers needing a definitive remote-durable barrier
// (ManualSync mode) use SyncNow, which blocks on the exclusive gate.
func (m *Syncer) Flush(ctx context.Context, payloadID string) (*block.FlushResult, error) {
	if err := m.checkReady(ctx); err != nil {
		return nil, err
	}

	// 1. Per-payload metadata quiesce: persist any FileBlock metadata
	//    that the local store has queued (queueFileBlockUpdate during
	//    rollup commit) so the mirror loop below sees the
	//    authoritative manifest for this payloadID. This call carries
	//    only metadata-level semantics — the data-side rollup runs on
	//    its own worker pool and is observed transitively when
	//    ListUnsynced walks the CAS chunk store.
	m.local.SyncFileBlocksForFile(ctx, payloadID)

	// 2. Mirror loop: enumerate every CAS hash present locally but not
	//    yet marked synced and copy it to remote, then MarkSynced.
	//    Local-only or remote-unhealthy: early-exit with Finalized=false.
	if m.remoteStore == nil || !m.IsRemoteHealthy() {
		return &block.FlushResult{Finalized: false}, nil
	}
	// Serialize the explicit mirror against any other explicit drain and against
	// the continuous upload dispatcher. The gate is taken non-blocking: if
	// another explicit drain holds it, or dispatcher uploads are in flight, this
	// returns Finalized=false rather than waiting.
	//
	// #670: the contention branch returns Finalized=false WITHOUT waiting. This
	// is intentional — blocking the explicit caller until concurrent uploads
	// finish could span seconds of remote I/O and translate into protocol-client
	// D-state. Callers MUST NOT spin-retry: see godoc above and the
	// Flusher.Flush contract in pkg/block.
	m.ensureGate()
	ok, err := m.gate.acquireExclusive(ctx, false)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &block.FlushResult{Finalized: false}, nil
	}
	defer m.gate.releaseExclusive()

	if err := m.mirrorOnce(ctx); err != nil {
		return nil, err
	}
	return &block.FlushResult{Finalized: true}, nil
}

// mirrorOnce performs a single mirror-loop pass: every CAS hash
// present locally but not yet marked synced is read out of the local
// store, written to remote, then MarkSynced'd. Caller MUST hold the
// upload gate's exclusive lock (acquireExclusive) so the pass does not
// race the continuous dispatcher.
//
// Ordering is Put-then-Mark. A crash between remote.Put and MarkSynced
// is safe because remote.Put is idempotent on (hash, identical bytes)
// per the unified Store contract; the next pass re-Puts the same
// hash and proceeds to MarkSynced. MarkSynced fires only after Put
// returns nil — Mark never precedes Put, so a marked-synced hash is
// always actually present remotely.
//
// snapshotHashStore is observed under the syncer's RWMutex (the field
// is written via SetSyncedHashStore). With no SyncedHashStore wired
// the mirror loop short-circuits to a no-op — there is no production
// path that wires a remote store without also wiring a
// SyncedHashStore, but the defensive guard keeps test fixtures simple.
func (m *Syncer) mirrorOnce(ctx context.Context) error {
	m.mu.RLock()
	hashStore := m.syncedHashStore
	m.mu.RUnlock()
	if hashStore == nil {
		return nil
	}
	m.ensureUploadLimiter()

	// Snapshot the pending set, then upload outside the lock. Hashes
	// added mid-pass surface on the NEXT pass (same snapshot-at-start
	// semantics the walk-based ListUnsynced had). A hash is removed from
	// the set only AFTER MarkSynced succeeds, so a crash between Put and
	// MarkSynced is safe (re-Put is idempotent; the startup reconcile
	// re-seeds the hash).
	m.pendingMu.Lock()
	if len(m.pendingHashes) == 0 {
		m.pendingMu.Unlock()
		return nil
	}
	snapshot := make([]block.ContentHash, 0, len(m.pendingHashes))
	for h := range m.pendingHashes {
		snapshot = append(snapshot, h)
	}
	m.pendingMu.Unlock()

	// Tracks whether at least one pending hash was retained-but-not-mirrored
	// because its local bytes were gone. The pass continues draining the
	// other hashes (one bad hash must not stall the rest), and reports this
	// via ErrChunkLostBeforeMirror AFTER the pass so Flush/SyncNow do not
	// claim durability while the periodic uploader can treat it as a
	// non-fatal retry-next-tick condition.
	var lostBeforeMirror atomic.Bool

	// Upload the snapshot concurrently, bounded by the shared uploadLimiter.
	// Each chunk is independent — a distinct CAS hash, an idempotent remote Put,
	// and a per-hash MarkSynced — and the path is network-latency-bound, so a
	// serial loop leaves the link idle (one in-flight S3 Put gave only ~2.7
	// MiB/s VM→fr-par; #1266). The limiter's window is fixed when
	// --parallel-uploads is pinned and resized by the adaptive controller
	// otherwise (#1407), so a long pass picks up window changes mid-flight. The
	// first fatal error cancels the group via gctx; in-flight Puts observe the
	// cancellation. Mirrors the bounded errgroup pattern used by warm().
	g, gctx := errgroup.WithContext(ctx)

	for _, hash := range snapshot {
		hash := hash
		// Block for a slot, but stop dispatching the moment the group is
		// failing/cancelled (Acquire returns gctx.Err() then).
		if err := m.uploadLimiter.Acquire(gctx); err != nil {
			break
		}
		g.Go(func() error {
			defer m.uploadLimiter.Release()
			return m.mirrorChunk(gctx, hashStore, hash, &lostBeforeMirror)
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}
	if lostBeforeMirror.Load() {
		// At least one pending hash was retained-but-not-mirrored: the
		// payload is not durable on remote. Flush/SyncNow propagate this;
		// the periodic uploader logs+swallows it (non-fatal, retry next
		// tick — the good hashes were still drained above).
		return block.ErrChunkLostBeforeMirror
	}
	return nil
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

// mirrorChunk uploads one pending CAS chunk to the remote store and marks it
// synced. It returns nil — recording lostBeforeMirror — when the local bytes
// vanished before upload, so one missing chunk does not fail the whole pass.
// It returns an error on local read failure, local bitrot (hash mismatch),
// remote Put failure, or MarkSynced failure. Safe for concurrent use:
// pendingHashes mutation is guarded by pendingMu, the metrics sink and remote
// store are concurrency-safe, and each call owns a distinct hash.
func (m *Syncer) mirrorChunk(ctx context.Context, hashStore metadata.SyncedHashStore, hash block.ContentHash, lostBeforeMirror *atomic.Bool) error {
	data, err := m.local.Get(ctx, hash)
	if errors.Is(err, block.ErrChunkNotFound) {
		// The local chunk is gone before we could mirror it. Retain the hash
		// for the next tick rather than dropping it — dropping silently
		// destroyed the only copy of never-mirrored data. If the chunk is
		// truly gone (e.g. an external delete), the next seedPendingFromDisk
		// / ListUnsynced drift reconcile walks disk, finds the chunk absent,
		// and stops re-seeding it, so the retry loop terminates naturally
		// instead of spinning forever.
		logger.Error("mirrorOnce: chunk lost locally before mirror — retained for retry",
			"hash", hash.String())
		lostBeforeMirror.Store(true)
		return nil
	}
	if err != nil {
		m.failedSyncs.Add(1)
		return fmt.Errorf("local get %s: %w", hash, err)
	}

	mx := m.dataplaneMetrics()

	// Re-hash fetched bytes before upload. Local bitrot, torn writes, or
	// hardware errors between rollup-time hashing and this read would
	// otherwise silently propagate corrupt bytes to remote and MarkSynced
	// them. Downstream verification via ReadBlockVerified is post-facto and
	// useless once the local copy is evicted, so refuse the upload here.
	rehashStart := time.Now()
	computed := block.ContentHash(blake3.Sum256(data))
	if mx != nil {
		mx.RecordRehash(time.Since(rehashStart))
	}
	if computed != hash {
		m.failedSyncs.Add(1)
		logger.Error("local corruption detected before mirror upload — refusing to upload",
			"hash", hash.String(),
			"computed", computed.String(),
			"bytes", len(data))
		return fmt.Errorf("%w on hash %s: computed %s (refusing upload)", block.ErrCASContentMismatch, hash.String(), computed.String())
	}

	uploadStart := time.Now()
	if mx != nil {
		mx.UploadStarted()
	}
	err = m.remoteStore.Put(ctx, hash, data)
	if mx != nil {
		mx.UploadFinished()
		result := "ok"
		if err != nil {
			result = "error"
		}
		mx.RecordUpload(len(data), result, time.Since(uploadStart))
	}
	if err != nil {
		m.failedSyncs.Add(1)
		// Feed the adaptive controller: an upload error in this interval signals
		// server pushback, so the controller backs the window off next tick.
		m.uploadErrWindow.Add(1)
		return fmt.Errorf("remote put %s: %w", hash, err)
	}
	// Count delivered bytes for the adaptive controller's goodput sample. The
	// bytes are on the wire regardless of MarkSynced, so charge them here.
	m.uploadedBytesWindow.Add(int64(len(data)))
	// PR3a: every chunk is still a standalone CAS object, so record a standalone
	// locator (PackID=="" → persisted in the legacy form). PR3b's packer will
	// record pack locators here instead.
	if err := hashStore.MarkSynced(ctx, hash, block.ChunkLocator{Length: int64(len(data))}); err != nil {
		m.failedSyncs.Add(1)
		return fmt.Errorf("mark synced %s: %w", hash, err)
	}
	m.pendingMu.Lock()
	if size, ok := m.pendingHashes[hash]; ok {
		delete(m.pendingHashes, hash)
		m.unsyncedBytes.Add(-size)
	}
	m.pendingMu.Unlock()
	m.publishQueueDepth()
	m.completedSyncs.Add(1)
	return nil
}

// PendingCount returns the number of CAS chunks present locally but not yet
// mirrored to remote — the live pending-upload backlog. Sourced from the
// addPendingHash/mirrorChunk set, which is the actual upload path (unlike the
// vestigial SyncQueue).
func (m *Syncer) PendingCount() int {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	return len(m.pendingHashes)
}

// SyncCounts returns lifetime (completed, failed) mirror counts: chunks that
// reached remote and chunks that failed a mirror attempt.
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
// Blocks are stored under content-addressed keys (cas/XX/YY/<hash>)
// so we resolve via FileBlock metadata: enumerate every block belonging
// to payloadID, find the highest-offset remote-mirrored chunk, and
// compute size = chunkOffset + chunk.DataSize. DataSize is stamped at
// rollup time, so no extra S3 round-trip is needed.
//
// (mirror loop): mirrorOnce writes to remote.Put + MarkSynced
// but never transitions FileBlock.State to BlockStateRemote (the row
// state remains Pending/Syncing for the life of the payload). The
// authoritative per-hash mirror signal is therefore SyncedHashStore —
// not FileBlock.State. Each candidate row is included only if
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

	blocks, err := m.fileBlockStore.ListFileBlocks(ctx, payloadID)
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

	// ListFileBlocks returns blocks ordered by absolute chunk offset.
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
// returns true. The mirror loop (mirrorOnce) does not transition
// FileBlock.State to BlockStateRemote, so the legacy State filter is no
// longer authoritative. If no SyncedHashStore is wired (test fixtures)
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

	blocks, err := m.fileBlockStore.ListFileBlocks(ctx, payloadID)
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
// refcount path (engine.Delete decrements RefCount per BlockRef hash and
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

// startPeriodicUploader launches the continuous upload dispatcher, the
// maintenance loop, and (in adaptive mode) the goodput controller, if not
// already running. Must be called with m.mu held.
func (m *Syncer) startPeriodicUploader(ctx context.Context) {
	if m.periodicStarted {
		return
	}
	// Manual-sync mode: durability is driven solely by explicit Flush, so the
	// background uploader (and its adaptive controller) must not run. This makes
	// Flush the single, deterministic mirror driver — required to observe
	// snapshot-bounded / crash-replay mirror semantics that a concurrent
	// uploader would otherwise race.
	if m.config.ManualSync {
		m.periodicStarted = true
		return
	}
	m.periodicStarted = true

	interval := m.config.UploadInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	// The dispatcher sustains upload concurrency in steady state (#1432); the
	// maintenance loop handles the slow housekeeping (FileBlock metadata flush,
	// drift reconcile, failed-upload requeue) the dispatcher does not.
	go m.uploadDispatcher(ctx)
	go m.maintenanceLoop(ctx, interval)

	// Adaptive mode only: launch the goodput controller that resizes the
	// upload window to saturate the uplink (#1407). Pinned --parallel-uploads
	// leaves uploadController nil and keeps the fixed window — publish that
	// fixed window once so the gauge reflects it instead of reading 0.
	if m.uploadController != nil {
		go m.runUploadController(ctx, uploadControlInterval)
	} else if mx := m.dataplaneMetrics(); mx != nil {
		mx.SetUploadWindow(m.uploadLimiter.Limit())
	}
}

// uploadControlInterval is how often the adaptive controller samples goodput
// and resizes the upload window. One second is long enough for several S3 Puts
// to complete (so the goodput sample is stable) yet short enough to ramp from
// the floor to the ceiling within a few seconds.
const uploadControlInterval = time.Second

// runUploadController is the adaptive upload-concurrency loop (#1407). Every
// interval it converts the bytes successfully uploaded since the last tick into
// a goodput sample, feeds it (with the interval's error flag) to the goodput
// controller, and applies the returned window to the shared uploadLimiter.
//
// Idle intervals (no bytes, nothing in flight, no error) are skipped entirely:
// feeding a zero-goodput sample during a write pause would otherwise be read as
// a collapse and shrink the window for no reason. Runs only when
// uploadController is non-nil (adaptive mode).
func (m *Syncer) runUploadController(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Publish the starting window so the metric is populated before the first
	// adjustment.
	if mx := m.dataplaneMetrics(); mx != nil {
		mx.SetUploadWindow(m.uploadLimiter.Limit())
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-ticker.C:
		}

		window, goodput, inflight, sawErr, acted := m.adaptiveUploadTick(interval)
		if !acted {
			continue
		}
		logger.Debug("adaptive upload window",
			"window", window,
			"inflight", inflight,
			"goodput_mibps", goodput/(1024*1024),
			"saw_error", sawErr)
	}
}

// adaptiveUploadTick performs one control step: it converts the bytes uploaded
// since the last tick into a goodput sample, feeds it (with the interval's
// error flag) to the goodput controller, and applies the resulting window to
// the upload limiter. It returns the new window, the measured goodput (bytes/s),
// the current in-flight count, the error flag, and acted=false for a skipped
// idle interval (no bytes, nothing in flight, no error) where there is no
// signal to act on. Extracted from the goroutine loop so the bytes→goodput→
// window glue is unit-testable without a clock.
func (m *Syncer) adaptiveUploadTick(interval time.Duration) (window int, goodput float64, inflight int, sawErr, acted bool) {
	bytes := m.uploadedBytesWindow.Swap(0)
	sawErr = m.uploadErrWindow.Swap(0) > 0
	// Peak in-flight over the interval tells window-limited from app-limited:
	// if uploads filled the window the goodput reflects the window, otherwise
	// the upstream pipeline was the constraint (see goodputController.observe).
	inflight = m.uploadLimiter.TakePeak()
	curWindow := m.uploadLimiter.Limit()

	if bytes == 0 && inflight == 0 && !sawErr {
		return curWindow, 0, inflight, false, false
	}

	windowLimited := inflight >= curWindow
	goodput = float64(bytes) / interval.Seconds()
	window = m.uploadController.observe(goodput, windowLimited, sawErr)
	m.uploadLimiter.SetLimit(window)
	if mx := m.dataplaneMetrics(); mx != nil {
		mx.SetUploadWindow(window)
	}
	return window, goodput, inflight, sawErr, true
}

// SyncNow triggers an immediate mirror-loop pass for every locally
// stored CAS chunk that has not yet been marked synced. Blocks until
// the pass completes or the context is cancelled. Returns nil on full
// success, ctx.Err() on cancellation, or a wrapped error from the
// mirror loop. Callers such as the REST /drain-uploads endpoint and
// Close() rely on this signal.
//
// Serializes against the continuous upload dispatcher (and any other explicit
// drain) via the upload gate, so the explicit mirror pass runs alone over a
// clean pending set.
func (m *Syncer) SyncNow(ctx context.Context) error {
	if m.remoteStore == nil {
		return nil
	}
	// Take the explicit-drain lock, blocking (ctx-aware) for any other explicit
	// drain to finish and for the continuous dispatcher's in-flight uploads to
	// quiesce, so mirrorOnce runs alone over a clean pending set.
	m.ensureGate()
	if _, err := m.gate.acquireExclusive(ctx, true); err != nil {
		return err
	}
	defer m.gate.releaseExclusive()

	// Flush queued FileBlock metadata to the store so the mirror loop
	// picks up recently rolled-up chunks.
	m.local.SyncFileBlocks(ctx)

	return m.mirrorOnce(ctx)
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
	if m.fileBlockStore == nil {
		return nil
	}
	enum, ok := m.fileBlockStore.(syncingEnumerator)
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
		if err := m.fileBlockStore.Put(ctx, fb); err != nil {
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

// syncingEnumerator is an optional capability a FileBlockStore may
// implement so the syncer's restart-recovery janitor can find stale
// Syncing rows without a full table scan.
type syncingEnumerator interface {
	EnumerateSyncingBlocks(ctx context.Context) ([]*block.FileBlock, error)
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
	m.local.SetEvictionEnabled(true)

	m.startHealthMonitor(ctx)
	m.startPeriodicUploader(ctx)

	logger.Info("Remote store attached, periodic syncer started")
	return nil
}
