package engine

import (
	"context"
	"fmt"
	gosync "sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
)

// defaultShutdownTimeout is the maximum time to wait for the transfer queue
// to finish processing during graceful shutdown.
const defaultShutdownTimeout = 30 * time.Second

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
