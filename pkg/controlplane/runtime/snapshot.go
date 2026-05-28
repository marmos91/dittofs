package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// CreateSnapshotOpts is the operator-facing configuration for one
// CreateSnapshot invocation. Zero-value (NoSyncGate=false, RetryOf="")
// requests the default behavior: a fresh UUID, full sync gate enabled.
// D-23-15.
type CreateSnapshotOpts struct {
	// NoSyncGate (D-23-11) skips DrainAllUploads + VerifyRemoteDurability.
	// Final state: ready + RemoteDurable=false. Phase 24 restore reads
	// RemoteDurable=false and refuses (or requires --force).
	NoSyncGate bool

	// RetryOf (D-23-10), when non-empty, reuses the failed snapshot's
	// ID + on-disk dir and atomically overwrites manifest.hashes via
	// WriteManifestAtomic. The target row must currently be in state
	// 'failed' — anything else returns ErrSnapshotRetryTargetNotFailed.
	RetryOf string
}

// CreateSnapshot orchestrates an asynchronous share snapshot. The call
// returns (snapID, nil) immediately after the state='creating' row is
// inserted (or, for RetryOf, flipped failed -> creating) AND the
// per-snapshot on-disk directory is created. The backup -> manifest ->
// drain -> verify -> ready/failed pipeline runs in a goroutine derived
// from r.runtimeCtx (NOT the caller's ctx, so adapter teardown does not
// kill in-flight snapshots — D-23-17).
//
// Synchronous failures returned to the caller:
//   - shares.ErrShareNotFound — unknown share
//   - ErrSnapshotBackupFailed wrap — share is memory-only (no local
//     store dir) OR metadata engine doesn't implement metadata.Backupable
//   - models.ErrSnapshotRetryTargetNotFound / models.ErrSnapshotRetryTargetNotFailed
//   - models.ErrSnapshotStateConflict — another in-flight snapshot
//     already exists for this share (Phase 22 D-08 partial unique index)
//   - wrapped fs error — snapshot directory could not be created
//
// Goroutine-only failures (observable via WaitForSnapshot in plan 23-06):
//   - models.ErrSnapshotBackupFailed — Backupable.Backup, dump write, or
//     manifest write failed
//   - models.ErrSnapshotDrainTimeout — DrainAllUploads returned a ctx error
//   - models.ErrSnapshotVerifyFailed — sync gate found missing hashes
//     even after one drain retry
//
// On goroutine failure, the row flips to state='failed', metadata.dump
// and manifest.hashes are retained on disk for retry (D-23-09), and the
// wrapped sentinel is posted to the per-snap result chan immediately
// before close.
func (r *Runtime) CreateSnapshot(ctx context.Context, shareName string, opts CreateSnapshotOpts) (string, error) {
	if r == nil || r.store == nil {
		return "", errors.New("runtime: nil store")
	}

	// (1) Resolve share -> local store dir. Memory-backed shares cannot
	// snapshot because there's nowhere to write the dump + manifest.
	localStoreDir, err := r.sharesSvc.LocalStoreDir(shareName)
	if err != nil {
		return "", err
	}
	if localStoreDir == "" {
		return "", fmt.Errorf("snapshot create %q: memory-only share has no local store dir: %w",
			shareName, models.ErrSnapshotBackupFailed)
	}

	// (2) Resolve metadata store + type-assert to Backupable.
	metaStore, err := r.GetMetadataStoreForShare(shareName)
	if err != nil {
		return "", err
	}
	backupable, ok := metaStore.(metadata.Backupable)
	if !ok {
		return "", fmt.Errorf("snapshot create %q: metadata engine does not implement Backupable: %w",
			shareName, models.ErrSnapshotBackupFailed)
	}

	// (3) Insert / flip the state='creating' row BEFORE any I/O (D-23-01).
	// The Phase 22 idx_share_creating partial unique index only enforces
	// concurrent-create rejection if the row exists during the second
	// Create call.
	var snap *models.Snapshot
	if opts.RetryOf != "" {
		// Retry path: look up + validate the failed target, then flip
		// state failed -> creating (D-23-10).
		existing, gerr := r.store.GetSnapshot(ctx, shareName, opts.RetryOf)
		if gerr != nil {
			if errors.Is(gerr, models.ErrSnapshotNotFound) {
				return "", fmt.Errorf("snapshot retry %q on share %q: %w",
					opts.RetryOf, shareName, models.ErrSnapshotRetryTargetNotFound)
			}
			return "", fmt.Errorf("snapshot retry: get snapshot %q: %w", opts.RetryOf, gerr)
		}
		if verr := snapshot.ValidateRetryTarget(existing); verr != nil {
			return "", verr
		}
		if uerr := r.store.UpdateSnapshotState(ctx, shareName, opts.RetryOf, models.StateCreating); uerr != nil {
			return "", fmt.Errorf("snapshot retry: flip failed->creating for %q: %w", opts.RetryOf, uerr)
		}
		snap = existing
		snap.State = models.StateCreating
	} else {
		// Fresh-create path: insert a new row.
		snap = &models.Snapshot{
			ID:        uuid.NewString(),
			ShareName: shareName,
			State:     models.StateCreating,
		}
		if _, cerr := r.store.CreateSnapshot(ctx, snap); cerr != nil {
			// ErrSnapshotStateConflict is surfaced as-is so callers can
			// errors.Is it.
			return "", cerr
		}
	}
	snapID := snap.ID

	// (4) Create on-disk dir. Failure here is synchronous — flip the row
	// to failed so the index slot is released for the next attempt.
	dir := snap.SnapshotDir(localStoreDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		_ = r.store.UpdateSnapshotState(ctx, shareName, snapID, models.StateFailed)
		return "", fmt.Errorf("snapshot create %q: mkdir %q: %w", snapID, dir, err)
	}

	// (5) Register the goroutine in the per-share registry under the
	// snapInFlight lock (D-23-17). Derive the child ctx from runtimeCtx
	// (not the caller's request ctx) so the orchestration outlives the
	// caller and dies promptly on Runtime shutdown.
	childCtx, doneCh, entry := r.registerSnapInFlight(shareName, snapID)

	// (6) Launch orchestration. Goroutine owns: backup, dump+manifest,
	// (optional) drain+verify, final state flip, posting to doneCh,
	// unregistering itself, wg.Done.
	go r.runSnapshotOrchestration(
		childCtx,
		shareName,
		snapID,
		localStoreDir,
		opts,
		backupable,
		doneCh,
		entry,
	)

	logger.Info("snapshot create: accepted",
		"snapshot_id", snapID,
		"share", shareName,
		"no_sync_gate", opts.NoSyncGate,
		"retry_of", opts.RetryOf,
	)
	return snapID, nil
}

// registerSnapInFlight allocates / reuses the per-share snapInFlight
// entry, derives a cancellable child ctx from r.runtimeCtx, appends the
// cancel func to the entry, records a buffered per-snap result channel,
// and increments the WaitGroup. Returns the child ctx + per-snap chan +
// the entry pointer (so the goroutine can call back into the entry for
// cleanup via unregisterSnap). D-23-17.
func (r *Runtime) registerSnapInFlight(shareName, snapID string) (context.Context, chan snapResult, *snapInFlight) {
	childCtx, cancel := context.WithCancel(r.runtimeCtx)
	doneCh := make(chan snapResult, 1)

	r.snapInFlightMu.Lock()
	entry, ok := r.snapInFlight[shareName]
	if !ok {
		entry = &snapInFlight{
			done: make(map[string]chan snapResult),
		}
		r.snapInFlight[shareName] = entry
	}
	r.snapInFlightMu.Unlock()

	entry.mu.Lock()
	entry.cancels = append(entry.cancels, cancel)
	entry.done[snapID] = doneCh
	entry.wg.Add(1)
	entry.mu.Unlock()

	return childCtx, doneCh, entry
}

// unregisterSnap removes the per-snap done channel from the share entry.
// The share entry itself is intentionally left in place even when empty
// — RemoveShare and Shutdown (plan 23-05) enumerate it and rely on
// wg.Wait. Leaving stale empty maps around is acceptable bookkeeping
// cost vs. the synchronization needed to delete on every snap completion.
func (r *Runtime) unregisterSnap(shareName, snapID string, entry *snapInFlight) {
	entry.mu.Lock()
	delete(entry.done, snapID)
	entry.mu.Unlock()
}

// runSnapshotOrchestration executes the per-snapshot pipeline on its own
// goroutine. terminalErr captures the wrapped sentinel (or nil for
// success) to post on doneCh in the deferred cleanup.
func (r *Runtime) runSnapshotOrchestration(
	ctx context.Context,
	shareName string,
	snapID string,
	localStoreDir string,
	opts CreateSnapshotOpts,
	backupable metadata.Backupable,
	doneCh chan snapResult,
	entry *snapInFlight,
) {
	var terminalErr error
	defer func() {
		// Non-blocking send (cap=1 chan), then close so subscribers see
		// "done" via a closed channel even if they were already past the
		// receive.
		doneCh <- snapResult{err: terminalErr}
		close(doneCh)
		r.unregisterSnap(shareName, snapID, entry)
		entry.wg.Done()
	}()

	logger.Debug("snapshot create: orchestration start",
		"snapshot_id", snapID,
		"share", shareName,
		"no_sync_gate", opts.NoSyncGate,
		"retry_of", opts.RetryOf,
	)

	// Reconstruct the snapshot model to derive on-disk paths. The state
	// CRUD methods only need (shareName, id) so we don't need to refetch.
	snap := &models.Snapshot{ID: snapID, ShareName: shareName}

	// --- Step 1: Backup -> metadata.dump (D-23-21 atomic temp+rename) ---
	dumpPath := snap.MetadataDumpPath(localStoreDir)
	logger.Debug("snapshot create: backup start",
		"snapshot_id", snapID,
		"share", shareName,
		"dump_path", dumpPath,
	)
	hashSet, err := snapshot.WriteMetadataDumpAtomic(dumpPath, func(w io.Writer) (*blockstore.HashSet, error) {
		return backupable.Backup(ctx, w)
	})
	if err != nil {
		r.failSnap(shareName, snapID)
		terminalErr = fmt.Errorf("snapshot create %s: backup: %w: %v",
			snapID, models.ErrSnapshotBackupFailed, err)
		logger.Error("snapshot create: backup failed",
			"snapshot_id", snapID,
			"share", shareName,
			"error", err,
		)
		return
	}
	manifestCount := 0
	if hashSet != nil {
		manifestCount = hashSet.Len()
	}
	logger.Info("snapshot create: backup complete",
		"snapshot_id", snapID,
		"share", shareName,
		"manifest_count", manifestCount,
	)

	// --- Step 2: Manifest write ---
	manifestPath := snap.ManifestPath(localStoreDir)
	if hashSet == nil {
		// Empty engine — synthesize an empty HashSet so the manifest
		// file exists. WriteManifestAtomic handles empty input as zero
		// bytes; the hold filter (D-23-02) still recognizes an empty
		// manifest as present.
		hashSet = blockstore.NewHashSet(0)
	}
	if err := snapshot.WriteManifestAtomic(manifestPath, hashSet); err != nil {
		r.failSnap(shareName, snapID)
		terminalErr = fmt.Errorf("snapshot create %s: write manifest: %w: %v",
			snapID, models.ErrSnapshotBackupFailed, err)
		logger.Error("snapshot create: manifest write failed",
			"snapshot_id", snapID,
			"share", shareName,
			"error", err,
		)
		return
	}
	logger.Info("snapshot create: manifest written",
		"snapshot_id", snapID,
		"share", shareName,
		"manifest_count", manifestCount,
	)

	// --- Step 3: NoSyncGate short-circuit (D-23-11) ---
	if opts.NoSyncGate {
		if err := r.store.UpdateSnapshotState(ctx, shareName, snapID, models.StateReady); err != nil {
			r.failSnap(shareName, snapID)
			terminalErr = fmt.Errorf("snapshot create %s: flip ready (no-sync-gate): %w: %v",
				snapID, models.ErrSnapshotBackupFailed, err)
			logger.Error("snapshot create: flip ready failed (no-sync-gate)",
				"snapshot_id", snapID, "share", shareName, "error", err)
			return
		}
		if err := r.store.UpdateSnapshotDurable(ctx, shareName, snapID, false); err != nil {
			// Best-effort log; state is already ready, so caller will
			// see ready+default(false). Don't fail.
			logger.Error("snapshot create: clear remote_durable failed",
				"snapshot_id", snapID, "share", shareName, "error", err)
		}
		logger.Info("snapshot create: ready (sync gate skipped)",
			"snapshot_id", snapID,
			"share", shareName,
			"final_state", "ready",
			"remote_durable", false,
		)
		return
	}

	// --- Step 4: Sync gate drain (D-23-05) ---
	bs, err := r.sharesSvc.GetBlockStoreForShare(shareName)
	if err != nil || bs == nil {
		r.failSnap(shareName, snapID)
		terminalErr = fmt.Errorf("snapshot create %s: no block store for share %q: %w",
			snapID, shareName, models.ErrSnapshotVerifyFailed)
		logger.Error("snapshot create: block store lookup failed",
			"snapshot_id", snapID, "share", shareName, "error", err)
		return
	}
	logger.Debug("snapshot create: drain start", "snapshot_id", snapID, "share", shareName)
	if err := bs.DrainAllUploads(ctx); err != nil {
		r.failSnap(shareName, snapID)
		sentinel := models.ErrSnapshotBackupFailed
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			sentinel = models.ErrSnapshotDrainTimeout
		}
		terminalErr = fmt.Errorf("snapshot create %s: drain: %w: %v", snapID, sentinel, err)
		logger.Error("snapshot create: drain failed",
			"snapshot_id", snapID, "share", shareName, "error", err)
		return
	}
	logger.Info("snapshot create: drain complete", "snapshot_id", snapID, "share", shareName)

	// --- Step 5: Verify ---
	remoteStore := bs.RemoteStore()
	if remoteStore == nil {
		r.failSnap(shareName, snapID)
		terminalErr = fmt.Errorf("snapshot create %s: share %q has no remote store, cannot verify: %w",
			snapID, shareName, models.ErrSnapshotVerifyFailed)
		logger.Error("snapshot create: no remote store",
			"snapshot_id", snapID, "share", shareName)
		return
	}
	concurrency := r.snapshotDefaults().SyncGateConcurrency
	logger.Debug("snapshot create: verify start",
		"snapshot_id", snapID, "share", shareName,
		"verify_concurrency", concurrency)
	verr := snapshot.VerifyRemoteDurability(ctx, remoteStore, hashSet, concurrency)
	if verr != nil && errors.Is(verr, blockstore.ErrBlockNotFound) {
		// One drain + re-verify retry (D-23-05). Common cause: syncer
		// was behind during the first verify; a fresh drain catches up.
		logger.Debug("snapshot create: verify miss, retrying drain+verify",
			"snapshot_id", snapID, "share", shareName, "first_error", verr)
		if derr := bs.DrainAllUploads(ctx); derr != nil {
			r.failSnap(shareName, snapID)
			sentinel := models.ErrSnapshotBackupFailed
			if errors.Is(derr, context.Canceled) || errors.Is(derr, context.DeadlineExceeded) {
				sentinel = models.ErrSnapshotDrainTimeout
			}
			terminalErr = fmt.Errorf("snapshot create %s: re-drain after verify miss: %w: %v",
				snapID, sentinel, derr)
			logger.Error("snapshot create: re-drain failed",
				"snapshot_id", snapID, "share", shareName, "error", derr)
			return
		}
		verr = snapshot.VerifyRemoteDurability(ctx, remoteStore, hashSet, concurrency)
	}
	if verr != nil {
		r.failSnap(shareName, snapID)
		terminalErr = fmt.Errorf("snapshot create %s: verify: %w: %v",
			snapID, models.ErrSnapshotVerifyFailed, verr)
		logger.Error("snapshot create: verify failed",
			"snapshot_id", snapID, "share", shareName, "error", verr)
		return
	}

	// --- Step 6: Ready flip (D-23-03) ---
	if err := r.store.UpdateSnapshotState(ctx, shareName, snapID, models.StateReady); err != nil {
		r.failSnap(shareName, snapID)
		terminalErr = fmt.Errorf("snapshot create %s: flip ready: %w: %v",
			snapID, models.ErrSnapshotBackupFailed, err)
		logger.Error("snapshot create: flip ready failed",
			"snapshot_id", snapID, "share", shareName, "error", err)
		return
	}
	if err := r.store.UpdateSnapshotDurable(ctx, shareName, snapID, true); err != nil {
		// State is already ready; durability flip failure is logged but
		// not fatal. Caller observing the row will see ready+default(false)
		// and can re-run the verify path explicitly if needed.
		logger.Error("snapshot create: set remote_durable=true failed",
			"snapshot_id", snapID, "share", shareName, "error", err)
	}
	logger.Info("snapshot create: ready",
		"snapshot_id", snapID,
		"share", shareName,
		"manifest_count", manifestCount,
		"verify_concurrency", concurrency,
		"final_state", "ready",
		"remote_durable", true,
	)
}

// failSnap flips the snapshot row to state='failed'. Best-effort: if the
// row update itself fails (e.g., DB unavailable), we log but do not
// double-fail the orchestration error — the wrapped sentinel posted to
// doneCh is still the authoritative signal for callers, and the
// startup-recovery scan (plan 23-05) will reconcile orphaned creating
// rows on the next restart.
//
// Uses context.Background so a cancelled parent ctx (the very common
// reason orchestration is bailing out) does not also prevent the failed
// flip.
func (r *Runtime) failSnap(shareName, snapID string) {
	if err := r.store.UpdateSnapshotState(context.Background(), shareName, snapID, models.StateFailed); err != nil {
		logger.Error("snapshot create: failed to flip state=failed (will reconcile on next restart)",
			"snapshot_id", snapID,
			"share", shareName,
			"error", err,
		)
	}
}

// cancelAndWaitInFlightSnaps cancels every in-flight snapshot orchestration
// goroutine for the given share and blocks until they all complete. Safe to
// call when no entry exists for the share (no-op). Called from
// Runtime.RemoveShare BEFORE delegating to sharesSvc.RemoveShare so the
// goroutines do not race the per-share snapshots/ tree wipe (Phase 22 D-15)
// or any in-flight metadata-store I/O. D-23-17.
//
// Lock discipline: snapInFlightMu is held only long enough to snapshot the
// cancel funcs + take a reference to the WaitGroup, then released BEFORE the
// wg.Wait. Per PATTERNS.md shared-pattern §lock-protected registry: never
// block under the registry mutex.
func (r *Runtime) cancelAndWaitInFlightSnaps(shareName string) {
	r.snapInFlightMu.Lock()
	entry, ok := r.snapInFlight[shareName]
	if !ok {
		r.snapInFlightMu.Unlock()
		return
	}
	// Remove the share entry from the map so subsequent CreateSnapshot
	// calls (if any race in despite the RemoveShare caller's contract)
	// allocate a fresh entry rather than reusing one we are about to drain.
	delete(r.snapInFlight, shareName)
	r.snapInFlightMu.Unlock()

	// Snapshot the cancel funcs under the per-entry mutex (separate from
	// the registry mutex), then release before draining the WaitGroup.
	entry.mu.Lock()
	cancels := append([]context.CancelFunc(nil), entry.cancels...)
	entry.mu.Unlock()

	logger.Info("snapshot lifecycle: cancelling in-flight snapshots",
		"share", shareName,
		"count", len(cancels),
	)
	for _, cancel := range cancels {
		cancel()
	}

	// Wait OUTSIDE the lock — goroutines need to acquire entry.mu inside
	// their cleanup path (unregisterSnap) to delete their done-chan entry.
	entry.wg.Wait()

	logger.Info("snapshot lifecycle: in-flight snapshots drained",
		"share", shareName,
	)
}

// shutdownSnapshots cancels all in-flight snapshot goroutines across all
// shares and waits (bounded by ctx) for them to drain. Called as the FIRST
// step of Runtime.Shutdown so snapshot orchestration cannot use-after-close
// the metadata stores or control-plane DB. D-23-17.
//
// Step 1 cancels runtimeCtx, which propagates to every child ctx derived in
// registerSnapInFlight — every orchestration goroutine then notices the
// cancellation at its next ctx-aware call (Backup, DrainAllUploads,
// VerifyRemoteDurability, UpdateSnapshotState). The failSnap helper uses
// context.Background, so the state=failed flip still completes even with
// runtimeCtx cancelled.
//
// If ctx fires before all goroutines exit, this function logs a warning and
// returns. Orphan goroutines will still exit on their own (runtimeCancel
// already fired) — they just may not have finished by the time the caller
// moves on to StopAllAdapters / CloseMetadataStores. Callers wanting a hard
// upper bound should pass context.WithTimeout(...); callers passing
// context.Background block until full drain.
func (r *Runtime) shutdownSnapshots(ctx context.Context) {
	// Step 1: cancel every child ctx derived from runtimeCtx. Idempotent:
	// second call is a no-op.
	if r.runtimeCancel != nil {
		r.runtimeCancel()
	}

	// Step 2: snapshot the per-share entries under the registry mutex,
	// then clear the map (lock-protected registry pattern).
	r.snapInFlightMu.Lock()
	entries := make([]*snapInFlight, 0, len(r.snapInFlight))
	for _, e := range r.snapInFlight {
		entries = append(entries, e)
	}
	r.snapInFlight = make(map[string]*snapInFlight)
	r.snapInFlightMu.Unlock()

	if len(entries) == 0 {
		logger.Info("snapshot lifecycle: no in-flight snapshots at shutdown")
		return
	}

	// Step 3: drain on a side goroutine so we can race ctx.Done.
	done := make(chan struct{})
	go func() {
		for _, e := range entries {
			e.wg.Wait()
		}
		close(done)
	}()

	select {
	case <-done:
		logger.Info("snapshot lifecycle: all snapshots drained",
			"share_count", len(entries),
		)
	case <-ctx.Done():
		logger.Warn("snapshot lifecycle: shutdownSnapshots ctx cancelled before all goroutines drained",
			"share_count", len(entries),
			"error", ctx.Err(),
		)
		// Do not block further; runtimeCancel already fired so the
		// remaining goroutines will exit on their own.
	}
}

// recoverOrphanedSnapshots scans every share for snapshot rows still in
// state='creating' and flips each to state='failed'. Called once from
// Runtime.Serve AFTER metadata-store registration but BEFORE adapters
// start serving, so the Phase 22 D-08 partial unique index (one
// concurrent 'creating' row per share) is free for new CreateSnapshot
// calls. D-23-18.
//
// Recovery is structured-log-only (no schema column): each flip emits a
// slog.Warn marker with reason="abandoned_at_startup" so an operator
// triaging post-crash state can grep the log to distinguish failures
// that happened pre-restart from ones in the current run. This matches
// D-23-09: the on-disk metadata.dump + manifest.hashes are retained
// (hold filter D-23-02 continues to protect their blocks), and the
// operator can retry via CreateSnapshot with opts.RetryOf set.
//
// Non-fatal: any per-share or per-snap error is logged + accumulated
// into firstErr; the scan continues so a single corrupt share row does
// not block startup. The eventual DeleteSnapshot path (when the
// operator decides) reconciles whatever is left.
func (r *Runtime) recoverOrphanedSnapshots(ctx context.Context) error {
	if r == nil || r.store == nil {
		return nil
	}

	shareNames := r.sharesSvc.ListShares()
	var firstErr error
	var sharesScanned, recovered, errs int

	for _, shareName := range shareNames {
		sharesScanned++
		snaps, err := r.store.ListSnapshots(ctx, shareName)
		if err != nil {
			logger.Error("snapshot recovery: list snapshots failed",
				"share", shareName,
				"error", err,
			)
			errs++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, snap := range snaps {
			if snap.State != models.StateCreating {
				continue
			}
			if uerr := r.store.UpdateSnapshotState(ctx, shareName, snap.ID, models.StateFailed); uerr != nil {
				logger.Error("snapshot recovery: flip to failed",
					"snapshot_id", snap.ID,
					"share", shareName,
					"error", uerr,
				)
				errs++
				if firstErr == nil {
					firstErr = uerr
				}
				continue
			}
			recovered++
			logger.Warn("snapshot recovery: abandoned creating snapshot flipped to failed",
				"snapshot_id", snap.ID,
				"share", shareName,
				"reason", "abandoned_at_startup",
			)
		}
	}

	logger.Info("snapshot recovery: complete",
		"shares_scanned", sharesScanned,
		"recovered", recovered,
		"errors", errs,
	)
	return firstErr
}

// Ensure unused-import safety: the shares package is referenced via the
// type returned by GetBlockStoreForShare and via ErrShareNotFound in
// caller code. Keep the alias here in case future refactors prune.
var _ = shares.ErrShareNotFound
