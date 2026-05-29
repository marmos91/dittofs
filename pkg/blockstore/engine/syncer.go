package engine

import (
	"context"
	"errors"
	"fmt"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	"github.com/marmos91/dittofs/pkg/metadata"
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
	// the syncer is one of the engine-internal
	// callers that still reaches into the wider EngineFileBlockStore
	// surface (GetFileBlock for dual-read resolve, ListFileBlocks for
	// GetFileSize/Exists). routes reads through
	// FileAttr.Blocks and lets us drop the wider interface.
	fileBlockStore blockstore.EngineFileBlockStore // Required: enables content-addressed deduplication

	// coordinator is the metadata-side seam the file-level dedup short-
	// circuit and (post-rollup) PersistFileBlocks hook consult. The
	// rollup-time persist call now fires from the local store's
	// ObjectIDPersister callback (engine.New installs a closure that
	// delegates here); the Syncer's mirror-loop Flush no longer drives
	// PersistFileBlocks directly. May be nil in unit tests; production
	// callers wire a real coordinator via SetCoordinator.
	coordinator MetadataCoordinator

	// syncedHashStore persists per-CAS-hash local→remote mirror state.
	// The mirror loop in Flush consumes ListUnsynced (which itself
	// filters via SyncedHashStore.IsSynced) and calls MarkSynced after
	// each successful remote.Put. May be nil in unit tests / local-only
	// fixtures; production callers wire a real store via
	// SetSyncedHashStore.
	syncedHashStore metadata.SyncedHashStore

	// bs is a back-reference to the owning BlockStore.
	// the file-level dedup short-circuit needs to reach
	// BlockStore.cache to fire InvalidateFile on orphaned speculative
	// chunks. Reading through the back-reference (rather than copying a
	// `cache` field on the Syncer at construction time) lets test code
	// swap `bs.cache = rec` after construction and still observe the
	// invalidation — mirrors the TestClose_ClosesCache pattern. May be
	// nil in pre-wiring tests; callers must nil-check before use.
	bs *BlockStore

	config SyncerConfig

	queue *SyncQueue // Transfer queue for non-blocking operations

	inFlight   map[string]*fetchResult // In-flight download dedup (store key -> broadcast)
	inFlightMu gosync.Mutex

	stopCh chan struct{} // Signals periodic uploader to stop
	closed bool
	mu     gosync.RWMutex

	periodicStarted bool        // true once periodicUploader goroutine is launched
	uploading       atomic.Bool // Guards against overlapping periodic upload ticks

	healthMonitor   *HealthMonitor           // Monitors remote store health (nil when no remote)
	onHealthChanged healthTransitionCallback // Callback invoked on health state transitions

	firstOfflineRead    atomic.Bool  // Tracks if WARN was already logged since last healthy->unhealthy transition
	offlineReadsBlocked atomic.Int64 // Count of read operations blocked by remote unavailability
}

// NewSyncer creates a new Syncer. The fileBlockStore is required for content-addressed dedup.
func NewSyncer(local local.LocalStore, remoteStore remote.RemoteStore, fileBlockStore blockstore.EngineFileBlockStore, config SyncerConfig) *Syncer {
	if fileBlockStore == nil {
		panic("fileBlockStore is required for Syncer")
	}
	if config.ParallelUploads <= 0 {
		config.ParallelUploads = DefaultParallelUploads
	}
	if config.ParallelDownloads <= 0 {
		config.ParallelDownloads = DefaultParallelDownloads
	}
	if config.PrefetchBlocks <= 0 {
		config.PrefetchBlocks = DefaultPrefetchBlocks
	}
	// — apply CAS-path defaults.
	// ClaimBatchSize default removed (field deleted).
	if config.UploadConcurrency <= 0 {
		config.UploadConcurrency = 8
	}
	if config.ClaimTimeout <= 0 {
		config.ClaimTimeout = 10 * time.Minute
	}

	m := &Syncer{
		local:          local,
		remoteStore:    remoteStore,
		fileBlockStore: fileBlockStore,
		config:         config,
		inFlight:       make(map[string]*fetchResult),
		stopCh:         make(chan struct{}),
	}

	queueConfig := DefaultSyncQueueConfig()
	queueConfig.Workers = config.ParallelUploads
	queueConfig.DownloadWorkers = config.ParallelDownloads
	m.queue = NewSyncQueue(m, queueConfig)

	return m
}

// Queue returns the transfer queue for stats inspection.
func (m *Syncer) Queue() *SyncQueue { return m.queue }

// SetCoordinator wires the MetadataCoordinator the file-level dedup
// short-circuit reaches into (FindByObjectID, GetFileObjectID
// IncrementRefCount). engine.New plumbs the BlockStore's coordinator
// into the syncer at construction. Idempotent.
func (m *Syncer) SetCoordinator(c MetadataCoordinator) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.coordinator = c
}

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
	return fmt.Errorf("remote store unavailable (offline for %s): %w", dur.Truncate(time.Second), blockstore.ErrRemoteUnavailable)
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
// per the unified BlockStore contract, so the next Flush pass re-Puts
// the same hash and proceeds to MarkSynced.
//
// Return contract — see blockstore.Flusher godoc for the full state
// machine and caller-retry guidance. In brief:
//   - Finalized=true, err=nil: durable on the configured remote.
//   - Finalized=false, err=nil: SOFT condition (no remote configured,
//     remote unhealthy, OR another in-flight mirror pass holds the
//     `uploading` CAS gate). Callers MUST NOT tight-loop retry — see
//     #670 below.
//   - err != nil: hard failure, do not retry until addressed.
//
// #670: callers driving NFS COMMIT or SMB Flush loops over this method
// must rate-limit retries on Finalized=false. The `uploading`
// CompareAndSwap gate below makes the EXPLICIT Flush caller lose every
// attempt that races the periodic uploader's tick, so a tight
// in-handler retry loop pegs the CPU without ever making progress and
// starves the uploading goroutine. Recommended pattern: surface the
// soft-fail to the protocol adapter and let the client drive the next
// attempt on its own schedule (e.g. NFSv3 reports the WRITE's
// "committed" enum as UNSTABLE rather than DATASYNC/FILESYNC so the
// client reissues COMMIT later; SMB Flush returns success after a
// bounded attempt) instead of spinning in-handler.
func (m *Syncer) Flush(ctx context.Context, payloadID string) (*blockstore.FlushResult, error) {
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
		return &blockstore.FlushResult{Finalized: false}, nil
	}
	// Serialize the explicit mirror against the periodic uploader's
	// tick body. Both paths take the uploading atomic gate; whichever
	// holds it runs and the other observes Finalized=false.
	//
	// #670: the contention branch returns Finalized=false WITHOUT
	// waiting for the in-flight pass to complete. This is intentional —
	// blocking the explicit caller until the periodic uploader's tick
	// finishes could span minutes of remote I/O and translate into
	// protocol-client D-state. Callers MUST NOT spin-retry: see godoc
	// above and the Flusher.Flush contract in pkg/blockstore.
	if !m.uploading.CompareAndSwap(false, true) {
		return &blockstore.FlushResult{Finalized: false}, nil
	}
	defer m.uploading.Store(false)

	if err := m.mirrorOnce(ctx); err != nil {
		return nil, err
	}
	return &blockstore.FlushResult{Finalized: true}, nil
}

// mirrorOnce performs a single mirror-loop pass: every CAS hash
// present locally but not yet marked synced is read out of the local
// store, written to remote, then MarkSynced'd. Caller MUST hold the
// uploading atomic gate.
//
// Ordering is Put-then-Mark. A crash between remote.Put and MarkSynced
// is safe because remote.Put is idempotent on (hash, identical bytes)
// per the unified BlockStore contract; the next pass re-Puts the same
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
	for hash, err := range m.local.ListUnsynced(ctx) {
		if err != nil {
			return fmt.Errorf("list unsynced: %w", err)
		}
		data, err := m.local.Get(ctx, hash)
		if err != nil {
			return fmt.Errorf("local get %s: %w", hash, err)
		}
		// Re-hash fetched bytes before upload. Local bitrot, torn
		// writes, or hardware errors between rollup-time hashing and
		// this read would otherwise silently propagate corrupt bytes
		// to remote and MarkSynced them. Downstream verification via
		// ReadBlockVerified is post-facto and useless once the local
		// copy is evicted, so refuse the upload here and surface an
		// error to the syncer loop.
		computed := blockstore.ContentHash(blake3.Sum256(data))
		if computed != hash {
			logger.Error("local corruption detected before mirror upload — refusing to upload",
				"hash", hash.String(),
				"computed", computed.String(),
				"bytes", len(data))
			return fmt.Errorf("%w on hash %s: computed %s (refusing upload)", blockstore.ErrCASContentMismatch, hash.String(), computed.String())
		}
		if err := m.remoteStore.Put(ctx, hash, data); err != nil {
			return fmt.Errorf("remote put %s: %w", hash, err)
		}
		if err := hashStore.MarkSynced(ctx, hash); err != nil {
			return fmt.Errorf("mark synced %s: %w", hash, err)
		}
	}
	return nil
}

// snapshotPendingBlockRefs returns the speculativeBlocks list +
// parallel blockStates slice for the trigger
// evaluation. Projection: ListFileBlocks(payloadID) yields the FastCDC
// chunker output already produced by the local-store rollup
// (pkg/blockstore/local/fs/rollup.go:rollupFile populates Pending
// FileBlocks at chunk boundaries with the BLAKE3-256 chunk hash as
// FileBlock.Hash). expects the trigger to fire only when
// every projected block is Pending; this helper returns the FULL
// projection (every block, regardless of state) so the trigger
// guard inside trySpeculativeFileLevelDedup can veto on any non-Pending
// row without an extra read.
//
// The returned slice is in block-index ascending order (= Offset
// ascending given Offset = blockIdx * BlockSize), so
// ComputeObjectID(refs) over the result matches the canonical Merkle
// root the post-Flush hook would compute over the same blocks once
// they reach Remote.
//
// Skips rows whose ID does not parse as a {payloadID}/{idx} pair —
// such rows are foreign to this payload (defense-in-depth; the
// per-payload index returned by ListFileBlocks should never include
// them but the parse check guarantees correctness if a future change
// widens the scan).
func (m *Syncer) snapshotPendingBlockRefs(ctx context.Context, payloadID string) ([]blockstore.BlockRef, []blockstore.BlockState, error) {
	blocks, err := m.fileBlockStore.ListFileBlocks(ctx, payloadID)
	if err != nil {
		return nil, nil, err
	}
	refs := make([]blockstore.BlockRef, 0, len(blocks))
	states := make([]blockstore.BlockState, 0, len(blocks))
	prefix := payloadID + "/"
	for _, fb := range blocks {
		// Defense-in-depth: skip rows that don't belong to this payload.
		// ListFileBlocks(payloadID) is per-payload, but the prefix check
		// guarantees correctness if a future change widens the scan.
		if len(fb.ID) <= len(prefix) || fb.ID[:len(prefix)] != prefix {
			continue
		}
		// writers encode the chunk's absolute byte Offset
		// directly in the trailing ID component (FastCDC chunk
		// boundaries do not align to BlockSize). Use the parsed value
		// as-is — do NOT multiply by BlockSize.
		chunkOffset, ok := blockstore.ParseChunkOffset(fb.ID)
		if !ok {
			continue
		}
		refs = append(refs, blockstore.BlockRef{
			Hash:   fb.Hash,
			Offset: chunkOffset,
			Size:   fb.DataSize,
		})
		states = append(states, fb.State)
	}
	return refs, states, nil
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
		chunkOffset, ok := blockstore.ParseChunkOffset(fb.ID)
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

// startPeriodicUploader launches the periodic uploader goroutine if not already running.
// Must be called with m.mu held.
func (m *Syncer) startPeriodicUploader(ctx context.Context) {
	if m.periodicStarted {
		return
	}
	m.periodicStarted = true

	interval := m.config.UploadInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	go m.periodicUploader(ctx, interval)
}

// SyncNow triggers an immediate mirror-loop pass for every locally
// stored CAS chunk that has not yet been marked synced. Blocks until
// the pass completes or the context is cancelled. Returns nil on full
// success, ctx.Err() on cancellation, or a wrapped error from the
// mirror loop. Callers such as the REST /drain-uploads endpoint and
// Close() rely on this signal.
//
// Serializes against the periodic uploader via the m.uploading atomic
// gate so the explicit drain and the periodic tick never both run the
// mirror loop concurrently.
func (m *Syncer) SyncNow(ctx context.Context) error {
	if m.remoteStore == nil {
		return nil
	}
	for !m.uploading.CompareAndSwap(false, true) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer m.uploading.Store(false)

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
		if fb.State != blockstore.BlockStateSyncing {
			continue
		}
		if !fb.LastSyncAttemptAt.IsZero() && fb.LastSyncAttemptAt.After(cutoff) {
			continue
		}
		fb.State = blockstore.BlockStatePending
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
	EnumerateSyncingBlocks(ctx context.Context) ([]*blockstore.FileBlock, error)
}

// periodicUploader runs every interval, scanning for blocks to upload.
// Uses an atomic guard to prevent overlapping ticks: if the previous upload
// batch is still running when the ticker fires, the tick is skipped. This
// prevents unbounded memory growth when uploads take longer than the interval
// (e.g., 8 blocks x 2-3s S3 upload = 16-24s, but interval is only 2s).
func (m *Syncer) periodicUploader(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	logger.Info("Periodic syncer started", "interval", interval, "upload_delay", m.config.UploadDelay)

	for {
		select {
		case <-ticker.C:
			if !m.canProcess(ctx) {
				logger.Info("Periodic syncer: canProcess=false, exiting")
				return
			}
			// Skip this tick if the previous upload batch is still running.
			// This prevents overlapping ticks from multiplying memory usage.
			if !m.uploading.CompareAndSwap(false, true) {
				logger.Debug("Periodic syncer: previous tick still running, skipping")
				continue
			}
			func() {
				defer m.uploading.Store(false)
				// Circuit breaker: skip uploads when remote store is unhealthy
				if !m.IsRemoteHealthy() {
					logger.Warn("Periodic syncer: remote unhealthy, skipping upload cycle",
						"outage_duration", m.RemoteOutageDuration(),
						"hint", "check S3 credentials, endpoint, and bucket configuration")
					return
				}
				m.syncLocalBlocks(ctx)
			}()
		case <-m.stopCh:
			logger.Info("Periodic syncer: stopCh received, exiting")
			return
		case <-ctx.Done():
			logger.Info("Periodic syncer: context cancelled, exiting")
			return
		}
	}
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
	m.local.SetEvictionEnabled(true)

	m.startHealthMonitor(ctx)
	m.startPeriodicUploader(ctx)

	logger.Info("Remote store attached, periodic syncer started")
	return nil
}
