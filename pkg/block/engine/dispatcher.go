package engine

import (
	"context"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// dispatcherHealthRecheck bounds how often the dispatcher re-checks remote
// health while parked during an outage, so a sick remote does not spin the
// dispatcher between the work-available signal and the health gate.
const dispatcherHealthRecheck = 500 * time.Millisecond

// uploadGate coordinates the continuous upload dispatcher against the explicit
// drain path (Flush / SyncNow / uploadBlock).
//
// The dispatcher keeps many per-chunk uploads in flight at once to saturate the
// uplink during steady streaming (#1432). The explicit drain runs mirrorOnce,
// which snapshots the pending set and reports durability/error for that
// snapshot. If both ran at once they would double-upload the same CAS chunks
// (wasting the very bandwidth we are trying to reclaim) and perturb mirrorOnce's
// snapshot accounting. The gate gives the explicit path exclusive use of the
// upload path: it blocks new dispatcher uploads and waits for in-flight ones to
// quiesce, then lets mirrorOnce run alone.
//
// Invariant used by the explicit path: when active == 0, no dispatcher upload is
// in flight, so the pending set is not being mutated by the dispatcher and
// mirrorOnce can snapshot it cleanly.
type uploadGate struct {
	mu       gosync.Mutex
	cond     *gosync.Cond
	explicit bool // an explicit drain holds (or is acquiring) exclusivity
	active   int  // dispatcher uploads currently in flight
}

func newUploadGate() *uploadGate {
	g := &uploadGate{}
	g.cond = gosync.NewCond(&g.mu)
	return g
}

// ensureGate lazily creates the upload gate for Syncers built directly in test
// fixtures rather than via NewSyncer (which always wires it). Idempotent under
// m.mu; mirrors the existing ensureUploadLimiter guard.
func (m *Syncer) ensureGate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gate == nil {
		m.gate = newUploadGate()
	}
}

// waitCtx parks on the cond until broadcast, returning early if ctx is
// cancelled. The caller must hold g.mu. A watcher goroutine broadcasts under
// g.mu on ctx.Done so a parked waiter cannot miss the wakeup (cond.Wait
// atomically releases g.mu, so broadcasting under the lock serializes against
// the release window). The watcher lives only for this wait.
func (g *uploadGate) waitCtx(ctx context.Context) {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			g.mu.Lock()
			g.cond.Broadcast()
			g.mu.Unlock()
		case <-done:
		}
	}()
	g.cond.Wait()
}

// beginDispatch registers one in-flight dispatcher upload, blocking while an
// explicit drain holds exclusivity. Returns false (registering nothing) if ctx
// is cancelled, so the dispatcher does not strand a slot on shutdown. Each
// successful beginDispatch must be paired with exactly one endDispatch.
func (g *uploadGate) beginDispatch(ctx context.Context) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.explicit {
		if ctx.Err() != nil {
			return false
		}
		g.waitCtx(ctx)
	}
	if ctx.Err() != nil {
		return false
	}
	g.active++
	return true
}

// endDispatch retires one in-flight dispatcher upload and wakes any explicit
// drain waiting for the dispatcher to quiesce.
func (g *uploadGate) endDispatch() {
	g.mu.Lock()
	g.active--
	g.cond.Broadcast()
	g.mu.Unlock()
}

// acquireExclusive takes the explicit-drain lock. When block is false and
// another explicit drain holds the lock — or dispatcher uploads are in flight —
// it returns (false, nil) immediately rather than waiting; this preserves the
// non-blocking, soft-fail contract of Flush/uploadBlock (#670). When block is
// true it waits (ctx-aware) for the explicit slot and then for dispatcher
// uploads to drain to zero. On ctx cancellation it returns (false, ctx.Err())
// without holding the lock.
func (g *uploadGate) acquireExclusive(ctx context.Context, block bool) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	for g.explicit {
		if !block {
			return false, nil
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		g.waitCtx(ctx)
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	g.explicit = true

	// Drain in-flight dispatcher uploads so mirrorOnce runs alone.
	for g.active > 0 {
		if !block {
			// Non-blocking caller cannot wait for the dispatcher to quiesce:
			// release the slot it just took and soft-fail.
			g.explicit = false
			g.cond.Broadcast()
			return false, nil
		}
		if ctx.Err() != nil {
			g.explicit = false
			g.cond.Broadcast()
			return false, ctx.Err()
		}
		g.waitCtx(ctx)
	}
	return true, nil
}

// releaseExclusive releases the explicit-drain lock and wakes the dispatcher.
func (g *uploadGate) releaseExclusive() {
	g.mu.Lock()
	g.explicit = false
	g.cond.Broadcast()
	g.mu.Unlock()
}

// dispatcherEnabled reports whether the continuous upload dispatcher runs for
// this syncer: it needs a remote to upload to and must stay off in manual-sync
// mode, where durability is driven solely by explicit Flush.
func (m *Syncer) dispatcherEnabled() bool {
	return m.hasRemote.Load() && !m.config.ManualSync
}

// currentHashStore returns the wired SyncedHashStore under the syncer RWMutex.
func (m *Syncer) currentHashStore() metadata.SyncedHashStore {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.syncedHashStore
}

// claimReady pops the next dispatchable hash from the ready queue and marks it
// in-flight, skipping entries that were synced out from under it (no longer
// pending) or are already being uploaded. Returns ok=false when no dispatchable
// work remains.
func (m *Syncer) claimReady() (block.ContentHash, bool) {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	for len(m.readyQ) > 0 {
		h := m.readyQ[0]
		m.readyQ = m.readyQ[1:]
		if len(m.readyQ) == 0 {
			m.readyQ = nil // release the backing array once fully drained
		}
		if _, pending := m.pendingHashes[h]; !pending {
			continue // already mirrored (e.g. via markFetchedSynced)
		}
		if _, busy := m.inflight[h]; busy {
			continue // already being uploaded by another worker
		}
		m.inflight[h] = struct{}{}
		return h, true
	}
	return block.ContentHash{}, false
}

// releaseInflight clears a hash's in-flight mark after its upload attempt
// completes (success or failure). The pending-set deletion on success happens in
// mirrorChunk; this only retires the in-flight claim so the dispatcher can
// re-attempt a failed hash on the next requeue.
func (m *Syncer) releaseInflight(h block.ContentHash) {
	m.pendingMu.Lock()
	delete(m.inflight, h)
	m.pendingMu.Unlock()
}

// requeueOrphans rebuilds the ready queue from the authoritative pending set
// (every pending hash not currently in flight). It re-queues hashes whose
// previous upload attempt failed — they stay in pendingHashes but were dropped
// from the ready queue when claimed — and seeds the queue for a dispatcher that
// started after the pending set was populated (e.g. SetRemoteStore). Run from
// the maintenance loop, so retries are paced at the upload interval rather than
// hot-looping. Signals the dispatcher when there is work.
func (m *Syncer) requeueOrphans() {
	m.pendingMu.Lock()
	q := make([]block.ContentHash, 0, len(m.pendingHashes))
	for h := range m.pendingHashes {
		if _, busy := m.inflight[h]; !busy {
			q = append(q, h)
		}
	}
	m.readyQ = q
	m.pendingMu.Unlock()
	if len(q) > 0 {
		m.signalWake()
	}
}

// publishQueueDepth updates the data-plane upload-queue-depth gauge to the live
// pending-set size. Called whenever the pending set changes so the gauge tracks
// real-time depth instead of freezing at a per-pass snapshot count (#1432).
func (m *Syncer) publishQueueDepth() {
	mx := m.dataplaneMetrics()
	if mx == nil {
		return
	}
	m.pendingMu.Lock()
	n := len(m.pendingHashes)
	m.pendingMu.Unlock()
	mx.SetUploadQueueDepth(n)
}

// stopRequested reports whether shutdown has begun (stopCh closed), without
// blocking. Close() closes stopCh BEFORE it drains, so the dispatcher uses this
// to avoid spawning new uploads once a drain/Close is in progress (which would
// otherwise race remote.Close()).
func (m *Syncer) stopRequested() bool {
	select {
	case <-m.stopCh:
		return true
	default:
		return false
	}
}

// waitForWork blocks until the ready queue is non-empty or the syncer is
// shutting down. Returns false on shutdown (stop signal or ctx cancellation).
// Shutdown wins over available work, so a non-empty readyQ at Close time does
// not keep the dispatcher spawning uploads.
func (m *Syncer) waitForWork(ctx context.Context) bool {
	if m.stopRequested() {
		return false
	}
	m.pendingMu.Lock()
	has := len(m.readyQ) > 0
	m.pendingMu.Unlock()
	if has {
		return true
	}
	select {
	case <-m.wake:
		return true
	case <-m.stopCh:
		return false
	case <-ctx.Done():
		return false
	}
}

// uploadDispatcher is the continuous steady-state uploader (#1432). It keeps the
// upload window full by claiming the next pending hash the instant a slot frees,
// with no per-pass barrier — so inflight tracks the adaptive window during
// streaming instead of collapsing to a single TCP stream. The adaptive
// controller resizes the window via uploadLimiter; this loop simply rides it.
//
// Concurrency: beginDispatch is taken BEFORE claiming a hash. A dispatcher
// paused inside beginDispatch (blocked while an explicit drain holds the gate)
// holds no claim and no slot. A dispatcher already past beginDispatch — blocked
// in uploadLimiter.Acquire or running its upload goroutine — has incremented the
// gate's active count and may have set inflight[h]; acquireExclusive waits for
// active==0, so an explicit drain cannot start until that goroutine's deferred
// endDispatch fires (i.e. DrainAllUploads on a saturated window can take up to
// window+1 goroutine lifetimes). Each upload runs in its own goroutine bounded
// by uploadLimiter; the gate's active count and the in-flight set are both
// retired when that goroutine finishes.
func (m *Syncer) uploadDispatcher(ctx context.Context) {
	logger.Info("Upload dispatcher started")
	for {
		if !m.waitForWork(ctx) {
			logger.Info("Upload dispatcher: shutting down")
			return
		}

		// Circuit breaker: park (without claiming) while the remote is
		// unhealthy so we do not burn attempts against a down endpoint.
		if !m.IsRemoteHealthy() {
			select {
			case <-time.After(dispatcherHealthRecheck):
			case <-m.stopCh:
				return
			case <-ctx.Done():
				return
			}
			continue
		}

		// Register against explicit drains before claiming work.
		if !m.gate.beginDispatch(ctx) {
			return
		}

		// beginDispatch may have blocked while Close()'s drain held the gate.
		// Close closes stopCh before draining, so if shutdown has begun do not
		// spawn a new upload — it could outlive syncer.Close() and race
		// remote.Close().
		if m.stopRequested() {
			m.gate.endDispatch()
			return
		}

		h, ok := m.claimReady()
		if !ok {
			m.gate.endDispatch()
			continue
		}

		hashStore := m.currentHashStore()
		if hashStore == nil {
			// No mirror-state oracle wired: nothing to do (matches mirrorOnce).
			m.releaseInflight(h)
			m.gate.endDispatch()
			continue
		}

		// Block for a window slot. This is the backpressure that holds inflight
		// at the adaptive window: when the window is full the dispatcher waits
		// here and resumes the instant an in-flight upload releases its slot.
		if err := m.uploadLimiter.Acquire(ctx); err != nil {
			m.releaseInflight(h)
			m.gate.endDispatch()
			return
		}

		go func(h block.ContentHash) {
			defer m.gate.endDispatch()
			defer m.uploadLimiter.Release()
			defer m.releaseInflight(h)

			// The queue-depth gauge is driven by the pending-set mutators
			// (addPendingHash / mirrorChunk success / markFetchedSynced); a
			// failed or lost upload leaves the pending set unchanged, so the
			// dispatcher does not republish here.
			var lost atomic.Bool
			if err := m.mirrorChunk(ctx, hashStore, h, &lost); err != nil {
				if ctx.Err() != nil {
					logger.Debug("Upload dispatcher: chunk upload aborted during shutdown", "hash", h.String(), "error", err)
				} else {
					// Retained in the pending set; the maintenance loop requeues
					// it for a later retry.
					logger.Warn("Upload dispatcher: chunk upload failed; will retry", "hash", h.String(), "error", err)
				}
			}
		}(h)
	}
}

// maintenanceLoop runs the slow periodic housekeeping that the steady-state
// dispatcher does not: it persists queued FileChunk metadata, periodically
// re-seeds the pending set from disk (drift reconcile), and requeues pending
// hashes whose upload attempt failed so they are retried at the upload interval.
// It never uploads — that is the dispatcher's job.
func (m *Syncer) maintenanceLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Re-seed the ready queue from any pending hashes that predate the
	// dispatcher (e.g. SetRemoteStore attached a remote after chunks were
	// already pending).
	m.requeueOrphans()

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
			m.requeueOrphans()
		case <-m.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}
