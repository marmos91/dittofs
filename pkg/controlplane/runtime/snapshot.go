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

	// (2b) Resolve the metadata-store engine type ("memory" | "badger" |
	// "postgres") so Snapshot.MetadataEngine can be populated on the
	// fresh-create row. The retry path inherits MetadataEngine from the
	// existing failed row, so it does not need to look this up. Phase 24
	// restore consumes MetadataEngine to dispatch the per-engine
	// Restoreable driver; an empty value would break that lookup.
	shareCfg, err := r.sharesSvc.GetShare(shareName)
	if err != nil {
		return "", err
	}
	metaStoreCfg, err := r.store.GetMetadataStore(ctx, shareCfg.MetadataStore)
	if err != nil {
		return "", fmt.Errorf("snapshot create %q: resolve metadata store config %q: %w",
			shareName, shareCfg.MetadataStore, err)
	}
	metadataEngine := metaStoreCfg.Type
	if metadataEngine == "" {
		return "", fmt.Errorf("snapshot create %q: metadata store %q has empty Type, cannot record engine: %w",
			shareName, shareCfg.MetadataStore, models.ErrSnapshotBackupFailed)
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
			ID:             uuid.NewString(),
			ShareName:      shareName,
			State:          models.StateCreating,
			MetadataEngine: metadataEngine,
		}
		if _, cerr := r.store.CreateSnapshot(ctx, snap); cerr != nil {
			// ErrSnapshotStateConflict is surfaced as-is so callers can
			// errors.Is it.
			return "", cerr
		}
	}
	snapID := snap.ID

	// (4) Register the goroutine in the per-share registry under the
	// snapInFlight lock BEFORE any RemoveShare-visible work (mkdir under
	// the snapshots tree). Deriving the registry entry from runtimeCtx
	// here closes the window where a concurrent RemoveShare ->
	// cancelAndWaitInFlightSnaps could observe an empty registry between
	// the DB insert at (3) and the registration call. Without this
	// early-register, RemoveShare would no-op, wipe the snapshots/ tree,
	// and the orchestration goroutine launched below would later write
	// dump + manifest into a directory that had been deleted.
	//
	// Lock ordering rule: any synchronous failure between here and the
	// `go r.runSnapshotOrchestration(...)` call MUST run
	// abortSnapInFlight to release the wg.Add(1) the registration took
	// and close+drain the doneCh, otherwise cancelAndWaitInFlightSnaps
	// would block forever waiting for the never-launched goroutine.
	// D-23-17.
	childCtx, doneCh, entry := r.registerSnapInFlight(shareName, snapID)

	// (5) Create on-disk dir. Failure here is synchronous — flip the row
	// to failed so the index slot is released for the next attempt, AND
	// release the in-flight registration so the lifecycle WaitGroup
	// decrements.
	dir := snap.SnapshotDir(localStoreDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		r.abortSnapInFlight(shareName, snapID, entry, doneCh)
		_ = r.store.UpdateSnapshotState(ctx, shareName, snapID, models.StateFailed)
		return "", fmt.Errorf("snapshot create %q: mkdir %q: %w", snapID, dir, err)
	}

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

// WaitForSnapshot blocks until the snapshot's orchestration goroutine
// completes (the per-snap result chan is signalled-and-closed by
// runSnapshotOrchestration) or ctx is cancelled, then returns the final
// snapshot record fetched via GetSnapshot.
//
// Return values:
//   - In-flight snapshot, orchestration succeeded: blocks until the
//     goroutine sends snapResult{err: nil} + close(doneCh), then returns
//     the final row (state=ready) with nil error.
//   - In-flight snapshot, orchestration failed: blocks until the
//     goroutine sends snapResult{err: wrappedSentinel} + close(doneCh),
//     then returns the row (state=failed) PLUS the wrapped error so
//     callers can errors.Is against the D-23-12 sentinels (e.g.
//     models.ErrSnapshotVerifyFailed).
//   - Already-complete snapshot (chan drained or removed from registry):
//     no chan present → falls through to GetSnapshot immediately with
//     nil orchestration error. The row state carries the outcome.
//   - ctx cancel during wait: returns nil, ctx.Err() without consulting
//     GetSnapshot.
//   - Unknown snapshot id: GetSnapshot returns
//     models.ErrSnapshotNotFound which propagates unchanged.
//
// Concurrency: the per-snap chan is buffered with cap 1 and closed after
// the single send (see runSnapshotOrchestration's deferred cleanup), so
// reads after the close yield the zero-value snapResult{} — the first
// reader observes the orchestration error and subsequent readers see the
// row state (which already reflects failure as state=failed). This
// single-broadcast behavior is acceptable for the current single-caller
// pattern; the multi-subscriber event-stream upgrade (sync.Cond) is
// listed as deferred per CONTEXT D-23-19 "WaitForSnapshot event-stream
// API for many subscribers".
//
// Plan: 23-06 / D-23-19.
func (r *Runtime) WaitForSnapshot(ctx context.Context, shareName, snapID string) (*models.Snapshot, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("runtime: nil store")
	}

	// Snapshot the per-snap done chan under the registry lock so the
	// goroutine cleanup (unregisterSnap) cannot race the lookup. If no
	// share entry or no per-snap chan exists, the orchestration is either
	// already-complete or the snapID was never in-flight in this process
	// — both cases fall through to a direct GetSnapshot.
	var (
		doneCh chan snapResult
		hasCh  bool
	)
	r.snapInFlightMu.Lock()
	if entry, ok := r.snapInFlight[shareName]; ok {
		entry.mu.Lock()
		doneCh, hasCh = entry.done[snapID]
		entry.mu.Unlock()
	}
	r.snapInFlightMu.Unlock()

	var orchErr error
	if hasCh {
		select {
		case res := <-doneCh:
			// nil err on success or the wrapped sentinel on failure.
			// Closed-then-drained chans yield the zero-value
			// snapResult{} → orchErr stays nil and the row state is the
			// authoritative outcome for late subscribers.
			orchErr = res.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	snap, gerr := r.store.GetSnapshot(ctx, shareName, snapID)
	if gerr != nil {
		// ErrSnapshotNotFound propagates as-is via Phase 22's wrapping
		// (errors.Is works through the wrap).
		return nil, gerr
	}
	return snap, orchErr
}

// registerSnapInFlight allocates / reuses the per-share snapInFlight
// entry, derives a cancellable child ctx from r.runtimeCtx, appends the
// cancel func to the entry, records a buffered per-snap result channel,
// and increments the WaitGroup. Returns the child ctx + per-snap chan +
// the entry pointer (so the goroutine can call back into the entry for
// cleanup via unregisterSnap). D-23-17.
//
// Publish-race safety: r.snapInFlightMu is held for the WHOLE function
// body, including the entry.mu critical section. cancelAndWaitInFlightSnaps
// snapshots the entry pointer under r.snapInFlightMu; if it wins the
// registry lock before this function, it observes "share not present"
// and is a no-op. If it loses the registry lock, it observes a fully
// populated entry (cancel appended + done chan written + wg.Add(1)
// already executed), so its subsequent entry.wg.Wait() blocks until the
// goroutine launched after we return drains. Without this ordering a
// concurrent share teardown could delete the share entry between the
// registry publish and the wg.Add, and wg.Wait() would return
// immediately while the freshly-launched goroutine continued running
// against a wiped snapshots tree.
//
// Lock ordering rule (PATTERNS §lock-protected registry): registry
// mutex outermost; per-entry mutex inside. cancelAndWaitInFlightSnaps
// honors the same ordering — it acquires r.snapInFlightMu, then
// entry.mu while snapshotting cancels — so there is no inversion. We
// don't block under r.snapInFlightMu either: every operation inside it
// is in-process and bounded.
func (r *Runtime) registerSnapInFlight(shareName, snapID string) (context.Context, chan snapResult, *snapInFlight) {
	childCtx, cancel := context.WithCancel(r.runtimeCtx)
	doneCh := make(chan snapResult, 1)

	r.snapInFlightMu.Lock()
	defer r.snapInFlightMu.Unlock()
	entry, ok := r.snapInFlight[shareName]
	if !ok {
		entry = &snapInFlight{
			done: make(map[string]chan snapResult),
		}
		r.snapInFlight[shareName] = entry
	}

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

// abortSnapInFlight releases a registry entry created via
// registerSnapInFlight when CreateSnapshot fails synchronously BEFORE
// launching the orchestration goroutine. It is the synchronous-failure
// twin of runSnapshotOrchestration's deferred cleanup: drop the per-snap
// done map entry, close the doneCh so any racing WaitForSnapshot
// observes the zero-value snapResult{}, and call wg.Done so
// cancelAndWaitInFlightSnaps does not block forever.
//
// Idempotent under the snapInFlight scheme — only invoked once per
// failure path between registerSnapInFlight and the goroutine launch in
// CreateSnapshot.
func (r *Runtime) abortSnapInFlight(shareName, snapID string, entry *snapInFlight, doneCh chan snapResult) {
	r.unregisterSnap(shareName, snapID, entry)
	close(doneCh)
	entry.wg.Done()
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
		// Atomic state+durable flip mirrors the sync-gated path. The
		// remote_durable value is explicit (false) here so the row's
		// durability bit is set in the same UPDATE as the state, not
		// left to the column default. This way the post-create row
		// state is fully deterministic on success and not subject to
		// schema-default drift.
		if err := r.store.MarkSnapshotReady(ctx, shareName, snapID, false); err != nil {
			r.failSnap(shareName, snapID)
			terminalErr = fmt.Errorf("snapshot create %s: mark ready (no-sync-gate): %w: %v",
				snapID, models.ErrSnapshotBackupFailed, err)
			logger.Error("snapshot create: mark ready failed (no-sync-gate)",
				"snapshot_id", snapID, "share", shareName, "error", err)
			return
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
	if verr != nil && errors.Is(verr, blockstore.ErrChunkNotFound) {
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
	// Atomically transition state=creating -> state=ready AND set
	// remote_durable=true in a single conditional UPDATE. A two-step
	// (state, durable) sequence would leave a transient window where a
	// crash mid-update produces ready+remote_durable=false — visually
	// indistinguishable from the intentional --no-sync-gate result and
	// a false negative for Phase 24 restore's durability gate.
	if err := r.store.MarkSnapshotReady(ctx, shareName, snapID, true); err != nil {
		r.failSnap(shareName, snapID)
		terminalErr = fmt.Errorf("snapshot create %s: mark ready: %w: %v",
			snapID, models.ErrSnapshotBackupFailed, err)
		logger.Error("snapshot create: mark ready failed",
			"snapshot_id", snapID, "share", shareName, "error", err)
		return
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
