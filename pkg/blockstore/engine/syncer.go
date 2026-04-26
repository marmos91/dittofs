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

// Syncer handles async local-to-remote transfers with eager upload,
// parallel download, prefetch, in-flight dedup, and content-addressed dedup.
type Syncer struct {
	local          local.LocalStore
	remoteStore    remote.RemoteStore
	fileBlockStore blockstore.FileBlockStore // Required: enables content-addressed deduplication
	config         SyncerConfig

	queue *SyncQueue // Transfer queue for non-blocking operations

	inFlight   map[string]*fetchResult // In-flight download dedup (store key -> broadcast)
	inFlightMu gosync.Mutex

	stopCh chan struct{} // Signals periodic uploader to stop
	closed bool
	mu     gosync.RWMutex

	periodicStarted bool        // true once periodicUploader goroutine is launched
	uploading       atomic.Bool // Guards against overlapping periodic upload ticks

	healthMonitor   *HealthMonitor           // Monitors remote store health (nil when no remote)
	onHealthChanged HealthTransitionCallback // Callback invoked on health state transitions

	firstOfflineRead    atomic.Bool  // Tracks if WARN was already logged since last healthy->unhealthy transition
	offlineReadsBlocked atomic.Int64 // Count of read operations blocked by remote unavailability
}

// NewSyncer creates a new Syncer. The fileBlockStore is required for content-addressed dedup.
func NewSyncer(local local.LocalStore, remoteStore remote.RemoteStore, fileBlockStore blockstore.FileBlockStore, config SyncerConfig) *Syncer {
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
	// Phase 11 Plan 02 (D-13/D-14/D-25) — apply CAS-path defaults.
	if config.ClaimBatchSize <= 0 {
		config.ClaimBatchSize = 32
	}
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

// SetHealthCallback sets the callback invoked when the remote store health state changes.
// If the HealthMonitor is already running, the callback is forwarded to it immediately.
func (m *Syncer) SetHealthCallback(fn HealthTransitionCallback) {
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
// First failure after a healthy->unhealthy transition logs at WARN level;
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

// Flush writes dirty in-memory blocks to local store (.blk files).
// Remote uploads happen asynchronously via the periodic uploader, so this
// returns without waiting for remote sync. This decouples NFS COMMIT latency
// from remote upload latency -- with a sufficiently large local store, remote write
// performance equals local performance. Remote latency only matters when
// backpressure kicks in (local store full) or on cold reads.
func (m *Syncer) Flush(ctx context.Context, payloadID string) (*blockstore.FlushResult, error) {
	if err := m.checkReady(ctx); err != nil {
		return nil, err
	}

	if _, err := m.local.Flush(ctx, payloadID); err != nil {
		return nil, fmt.Errorf("local store flush failed: %w", err)
	}

	return &blockstore.FlushResult{Finalized: false}, nil
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
// Phase 11 (CAS): blocks are stored under content-addressed keys
// (cas/XX/YY/<hash>), so the legacy {payloadID}/block-{N} prefix scan no
// longer finds them. We resolve via FileBlock metadata: enumerate every
// block belonging to payloadID, find the highest-indexed Remote block,
// and compute size = maxIdx*BlockSize + lastBlock.DataSize. DataSize is
// stamped by uploadOne before flipping State to Remote, so no extra S3
// round-trip is needed.
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

	// ListFileBlocks returns blocks ordered by block index. Walk from the
	// end to find the highest-indexed Remote block (skip any trailing
	// Pending/Syncing rows that haven't been confirmed in the remote
	// store yet — they MUST NOT contribute to the remote-side size).
	for i := len(blocks) - 1; i >= 0; i-- {
		fb := blocks[i]
		if fb.State != blockstore.BlockStateRemote {
			continue
		}
		_, blockIdx, ok := parseBlockID(fb.ID, payloadID)
		if !ok {
			continue
		}
		return blockIdx*uint64(BlockSize) + uint64(fb.DataSize), nil
	}
	return 0, nil
}

// Exists checks if any blocks exist for a file in the remote store.
//
// Phase 11 (CAS): metadata-driven existence check. Returns true iff at
// least one FileBlock for payloadID has reached BlockStateRemote (i.e.
// the engine has confirmed at least one PUT into CAS storage). Pending
// and Syncing rows do NOT count — those are still local-only.
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
	for _, fb := range blocks {
		if fb.State == blockstore.BlockStateRemote {
			return true, nil
		}
	}
	return false, nil
}

// parseBlockID extracts the block index from a FileBlock.ID of the form
// "{payloadID}/{blockIdx}". Returns (payloadID, blockIdx, true) on a
// well-formed match for the expected payloadID; (_, 0, false) otherwise.
func parseBlockID(blockID, expectedPayloadID string) (string, uint64, bool) {
	prefix := expectedPayloadID + "/"
	if len(blockID) <= len(prefix) || blockID[:len(prefix)] != prefix {
		return "", 0, false
	}
	var idx uint64
	for _, c := range blockID[len(prefix):] {
		if c < '0' || c > '9' {
			return "", 0, false
		}
		idx = idx*10 + uint64(c-'0')
	}
	return expectedPayloadID, idx, true
}

// Truncate removes blocks beyond the new size from the remote store.
func (m *Syncer) Truncate(ctx context.Context, payloadID string, newSize uint64) error {
	if err := m.checkReady(ctx); err != nil {
		return err
	}
	if m.remoteStore == nil {
		logger.Debug("syncer: skipping Truncate, no remote store")
		return nil
	}
	// Health gate: skip remote cleanup when unhealthy. Local cache is the
	// source of truth for metadata; remote orphans are cleaned up later.
	if !m.IsRemoteHealthy() {
		logger.Warn("Truncate: skipping remote cleanup, remote store unhealthy",
			"payloadID", payloadID, "newSize", newSize)
		return nil
	}

	prefix := payloadID + "/"
	if newSize == 0 {
		return m.remoteStore.DeleteByPrefix(ctx, prefix)
	}

	keepBlockIdx := (newSize - 1) / uint64(BlockSize)

	blocks, err := m.remoteStore.ListByPrefix(ctx, prefix)
	if err != nil {
		return fmt.Errorf("list blocks: %w", err)
	}

	for _, bk := range blocks {
		pid, blockIdx, ok := blockstore.ParseStoreKey(bk)
		if !ok || pid != payloadID {
			continue
		}
		if blockIdx > keepBlockIdx {
			if err := m.remoteStore.DeleteBlock(ctx, bk); err != nil {
				return fmt.Errorf("delete block %s: %w", bk, err)
			}
		}
	}

	return nil
}

// Delete removes all blocks for a file from the remote store.
func (m *Syncer) Delete(ctx context.Context, payloadID string) error {
	if err := m.checkReady(ctx); err != nil {
		return err
	}

	if m.remoteStore == nil {
		logger.Debug("syncer: skipping Delete, no remote store")
		return nil
	}
	// Health gate: skip remote cleanup when unhealthy. Remote blocks become
	// orphans that garbage collection will clean up after recovery.
	if !m.IsRemoteHealthy() {
		logger.Warn("Delete: skipping remote cleanup, remote store unhealthy",
			"payloadID", payloadID)
		return nil
	}

	return m.remoteStore.DeleteByPrefix(ctx, payloadID+"/")
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

	// Phase 11 D-14: one-shot janitor pass before the periodic uploader
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

// SyncNow triggers an immediate upload cycle for all local blocks,
// bypassing any age filter. Blocks until all eligible blocks are uploaded
// or the context is cancelled. Returns nil on full success, ctx.Err() on
// cancellation, or a joined error listing every block that failed to upload —
// callers such as the REST /drain-uploads endpoint and Close() rely on this
// signal.
//
// Phase 11 Plan 02 (D-13/D-15/D-25): each cycle calls claimBatch to flip up
// to ClaimBatchSize Pending rows to Syncing via per-row PutFileBlock writes
// (no batched/transactional FileBlockStore API exists today — Phase 12+).
// Each row's CAS Pending→Syncing flip is atomic on its own, but the batch
// is NOT collectively atomic; a syncer crash mid-batch leaves a mix of
// Syncing and Pending rows. CAS idempotency tolerates that: on restart,
// reconciler reclaims orphaned Syncing rows back to Pending, and a second
// uploadOne over the same hash is a no-op against the immutable CAS object.
// A bounded pool of UploadConcurrency goroutines then drives uploadOne for
// every claimed block. The cycle repeats until claimBatch returns an empty
// batch (no more Pending work).
//
// SyncNow also serializes against the periodic uploader via the m.uploading
// gate so concurrent callers do not double-claim the same row.
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

	// Flush queued FileBlock metadata to the store so ListLocalBlocks can find them.
	m.local.SyncFileBlocks(ctx)

	var uploadErrs []error
	for {
		if err := ctx.Err(); err != nil {
			if len(uploadErrs) > 0 {
				return errors.Join(append(uploadErrs, err)...)
			}
			return err
		}
		batch, err := m.claimBatch(ctx, m.config.ClaimBatchSize)
		if err != nil {
			return fmt.Errorf("claim batch: %w", err)
		}
		if len(batch) == 0 {
			break
		}

		// Bounded share-wide upload pool (D-25).
		sem := make(chan struct{}, m.config.UploadConcurrency)
		var wg gosync.WaitGroup
		var errMu gosync.Mutex
		for _, fb := range batch {
			fb := fb
			if fb.LocalPath == "" {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				if err := m.uploadOne(ctx, fb); err != nil {
					// Per D-14, the row is left in Syncing on failure —
					// the janitor (or the next SyncNow after ClaimTimeout)
					// will requeue it. Logged at DEBUG.
					logger.Debug("uploadOne failed; row remains Syncing",
						"blockID", fb.ID, "error", err)
					errMu.Lock()
					uploadErrs = append(uploadErrs, err)
					errMu.Unlock()
				}
			}()
		}
		wg.Wait()
	}
	return errors.Join(uploadErrs...)
}

// claimBatch flips up to max Pending blocks to Syncing via per-row
// PutFileBlock writes (FileBlockStore exposes no batched/transactional
// claim API today; Phase 12+). Each row's transition is atomic on its own
// but the batch is NOT collectively atomic — a syncer crash mid-batch
// leaves a mix of Syncing and Pending rows. CAS idempotency tolerates the
// resulting partial state: recoverStaleSyncing returns the abandoned
// Syncing rows to Pending, and any duplicate uploadOne over the same hash
// is a no-op against the immutable CAS object. Stamps
// LastSyncAttemptAt = now on every claimed row.
//
// Serialization scope (D-13): WITHIN ONE syncer instance, PutFileBlock is
// applied per row before the next iteration sees it, so two concurrent
// claimBatch callers in the same process cannot both observe + claim the
// same row (the m.uploading gate also serializes SyncNow against the
// periodic uploader at a coarser layer).
//
// ACROSS syncer instances (multi-process / multi-node) this method does
// NOT serialize: two syncers can each ListLocalBlocks the same Pending
// row before either calls PutFileBlock, both flip it to Syncing, and
// both upload. This is TOLERATED because CAS keys are content-defined
// (D-11 / INV-03) — the duplicate PUT is byte-identical to the same
// key and the second PutFileBlock simply overwrites the first with the
// same payload. A future ChangeStream / SELECT FOR UPDATE / WHERE-state
// guard could close the cross-process window without coordination, but
// the CAS idempotency makes the current contract correct.
func (m *Syncer) claimBatch(ctx context.Context, max int) ([]*blockstore.FileBlock, error) {
	if max <= 0 {
		max = m.config.ClaimBatchSize
	}
	pending, err := m.fileBlockStore.ListLocalBlocks(ctx, 0, max)
	if err != nil {
		return nil, fmt.Errorf("list local blocks: %w", err)
	}
	if len(pending) == 0 {
		return nil, nil
	}
	now := time.Now()
	claimed := make([]*blockstore.FileBlock, 0, len(pending))
	for _, fb := range pending {
		if fb == nil || fb.State != blockstore.BlockStatePending {
			continue
		}
		fb.State = blockstore.BlockStateSyncing
		fb.LastSyncAttemptAt = now
		if err := m.fileBlockStore.PutFileBlock(ctx, fb); err != nil {
			return nil, fmt.Errorf("claim block %s: %w", fb.ID, err)
		}
		claimed = append(claimed, fb)
	}
	return claimed, nil
}

// recoverStaleSyncing requeues blocks left in Syncing by a previous run
// (e.g., process killed mid-upload). Per D-14, any Syncing row whose
// LastSyncAttemptAt is older than cfg.ClaimTimeout is flipped back to
// Pending with LastSyncAttemptAt cleared. CAS idempotency makes the
// re-upload safe even if the original upload eventually completes — both
// writes target byte-identical bytes at byte-identical keys.
//
// Backends that opt in to syncingEnumerator return precise candidates;
// others degrade to a no-op (safe — claimBatch will not double-claim a
// Syncing row).
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
		if err := m.fileBlockStore.PutFileBlock(ctx, fb); err != nil {
			// Phase 11 IN-02: elevate per-row failure to ERROR and track
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
