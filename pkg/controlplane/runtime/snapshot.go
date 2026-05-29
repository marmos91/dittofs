package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// CreateSnapshotOpts is the operator-facing configuration for one
// CreateSnapshot invocation. Zero-value (NoVerify=false, RetryOf="")
// requests the default behavior: a fresh UUID, verify gate enabled.
type CreateSnapshotOpts struct {
	// Name is an optional human-readable label persisted on the snapshot
	// row. Empty leaves the column empty. Ignored on the RetryOf path,
	// which inherits the original row's Name.
	Name string

	// NoVerify skips DrainAllUploads + VerifyRemoteDurability (the verify
	// gate). Final state: ready with RemoteDurable=false. Restore reads
	// RemoteDurable=false and refuses unless AllowNonDurable is set.
	NoVerify bool

	// RetryOf, when non-empty, reuses the failed snapshot's ID + on-disk
	// dir and atomically overwrites manifest.hashes via
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
// kill in-flight snapshots).
//
// Synchronous failures returned to the caller:
//   - shares.ErrShareNotFound — unknown share
//   - ErrSnapshotBackupFailed wrap — share is memory-only (no local
//     store dir) OR metadata engine doesn't implement metadata.Backupable
//   - models.ErrSnapshotRetryTargetNotFound / models.ErrSnapshotRetryTargetNotFailed
//   - models.ErrSnapshotStateConflict — another in-flight snapshot
//     already exists for this share (partial unique index)
//   - wrapped fs error — snapshot directory could not be created
//
// Goroutine-only failures (observable via WaitForSnapshot):
//   - models.ErrSnapshotBackupFailed — Backupable.Backup, dump write, or
//     manifest write failed
//   - models.ErrSnapshotDrainTimeout — DrainAllUploads returned a ctx error
//   - models.ErrSnapshotVerifyFailed — sync gate found missing hashes
//     even after one drain retry
//
// On goroutine failure, the row flips to state='failed', metadata.dump
// and manifest.hashes are retained on disk for retry, and the wrapped
// sentinel is posted to the per-snap result chan immediately before close.
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
		return "", fmt.Errorf("snapshot create %q: in-memory local store has no on-disk root: %w",
			shareName, models.ErrSnapshotLocalStoreUnsupported)
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
	// existing failed row, so it does not need to look this up. Restore
	// consumes MetadataEngine to dispatch the per-engine Restoreable
	// driver; an empty value would break that lookup.
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

	// (3) Insert / flip the state='creating' row BEFORE any I/O. The
	// idx_share_creating partial unique index only enforces
	// concurrent-create rejection if the row exists during the second
	// Create call.
	var snap *models.Snapshot
	if opts.RetryOf != "" {
		// Retry path: look up + validate the failed target, then flip
		// state failed -> creating.
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
			Name:           opts.Name,
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
		"no_verify", opts.NoVerify,
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
//     callers can errors.Is against the typed sentinels (e.g.
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
// pattern; a multi-subscriber event-stream upgrade (sync.Cond) is a
// possible future enhancement.
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
		// ErrSnapshotNotFound propagates as-is (errors.Is works through
		// the underlying wrap).
		return nil, gerr
	}
	return snap, orchErr
}

// registerSnapInFlight allocates / reuses the per-share snapInFlight
// entry, derives a cancellable child ctx from r.runtimeCtx, appends the
// cancel func to the entry, records a buffered per-snap result channel,
// and increments the WaitGroup. Returns the child ctx + per-snap chan +
// the entry pointer (so the goroutine can call back into the entry for
// cleanup via unregisterSnap).
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
	if ok {
		// An entry observed mid-drain (cancelAndWaitInFlightSnaps has
		// flipped draining=true but not yet wg.Wait'd) must not be reused:
		// reusing it would attach the new snap to the wg the drainer is
		// already waiting on, deadlocking shutdown. Replace the map slot
		// with a fresh entry; the drainer still holds the original pointer
		// locally so its wg.Wait remains valid against the old entry.
		entry.mu.Lock()
		draining := entry.draining
		entry.mu.Unlock()
		if draining {
			ok = false
		}
	}
	if !ok {
		entry = &snapInFlight{
			cancels: make(map[string]context.CancelFunc),
			done:    make(map[string]chan snapResult),
		}
		r.snapInFlight[shareName] = entry
	}

	entry.mu.Lock()
	entry.cancels[snapID] = cancel
	entry.done[snapID] = doneCh
	entry.wg.Add(1)
	entry.mu.Unlock()

	return childCtx, doneCh, entry
}

// unregisterSnap removes the per-snap done channel and cancel func from
// the share entry. The cancel func is invoked here (cheap on an
// already-completed ctx) and deleted so the derived ctx is released from
// runtimeCtx's child list — otherwise completed snapshot contexts would
// pile up on runtimeCtx and entry.cancels would grow for the lifetime
// of the share.
//
// The share entry itself is intentionally left in place even when empty
// — RemoveShare and Shutdown enumerate it and rely on wg.Wait. Leaving
// stale empty maps around is acceptable bookkeeping cost vs. the
// synchronization needed to delete on every snap completion.
func (r *Runtime) unregisterSnap(shareName, snapID string, entry *snapInFlight) {
	entry.mu.Lock()
	if cancel, ok := entry.cancels[snapID]; ok {
		cancel()
		delete(entry.cancels, snapID)
	}
	delete(entry.done, snapID)
	entry.mu.Unlock()
}

// deriveWaitCtx returns a ctx rooted at r.runtimeCtx (so it is cancelled on
// runtime shutdown but NOT by the caller's request cancellation, e.g. an
// HTTP client disconnect) while still honoring the caller ctx's deadline if
// it has one. The returned cancel func must always be called to release
// resources. Used by RestoreSnapshot to wait for the safety snapshot
// without letting a disconnect abandon the wait.
func (r *Runtime) deriveWaitCtx(caller context.Context) (context.Context, context.CancelFunc) {
	root := r.runtimeCtx
	if root == nil {
		root = context.Background()
	}
	if dl, ok := caller.Deadline(); ok {
		return context.WithDeadline(root, dl)
	}
	return context.WithCancel(root)
}

// isSnapInFlight reports whether an orchestration goroutine for
// (shareName, snapID) is currently registered (create or retry in
// progress). Used by DeleteSnapshot to fence the delete against a running
// orchestration. Honors the registry lock ordering: registry mutex
// outermost, per-entry mutex inside.
func (r *Runtime) isSnapInFlight(shareName, snapID string) bool {
	r.snapInFlightMu.Lock()
	defer r.snapInFlightMu.Unlock()
	entry, ok := r.snapInFlight[shareName]
	if !ok {
		return false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	_, inFlight := entry.done[snapID]
	return inFlight
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
		"no_verify", opts.NoVerify,
		"retry_of", opts.RetryOf,
	)

	// Reconstruct the snapshot model to derive on-disk paths. The state
	// CRUD methods only need (shareName, id) so we don't need to refetch.
	snap := &models.Snapshot{ID: snapID, ShareName: shareName}

	// --- Step 0: Drain rollups BEFORE Backup ---
	// Force every dirty append-log payload through rollup into CAS + the
	// FileBlock manifest so the Backup() below sees a fully-populated
	// FileAttr.Blocks. Without this, a snapshot taken before the async
	// rollup catches up captures an empty/partial manifest.
	// Resolve the block store once here; the verify gate (Step 4) reuses
	// the same lookup pattern.
	bs, err := r.sharesSvc.GetBlockStoreForShare(shareName)
	if err != nil || bs == nil {
		terminalErr = fmt.Errorf("snapshot create %s: no block store for share %q: %w",
			snapID, shareName, models.ErrSnapshotBackupFailed)
		r.failSnap(shareName, snapID, terminalErr)
		logger.Error("snapshot create: block store lookup failed (pre-backup)",
			"snapshot_id", snapID, "share", shareName, "error", err)
		return
	}
	logger.Debug("snapshot create: drain rollups start", "snapshot_id", snapID, "share", shareName)
	if derr := bs.DrainRollups(ctx); derr != nil {
		terminalErr = fmt.Errorf("snapshot create %s: drain rollups: %w: %v",
			snapID, models.ErrSnapshotBackupFailed, derr)
		r.failSnap(shareName, snapID, terminalErr)
		logger.Error("snapshot create: drain rollups failed",
			"snapshot_id", snapID, "share", shareName, "error", derr)
		return
	}
	logger.Debug("snapshot create: drain rollups complete", "snapshot_id", snapID, "share", shareName)

	// --- Step 1: Backup -> metadata.dump (atomic temp+rename) ---
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
		terminalErr = fmt.Errorf("snapshot create %s: backup: %w: %v",
			snapID, models.ErrSnapshotBackupFailed, err)
		r.failSnap(shareName, snapID, terminalErr)
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
		// bytes; the hold filter still recognizes an empty manifest as
		// present.
		hashSet = blockstore.NewHashSet(0)
	}
	if err := snapshot.WriteManifestAtomic(manifestPath, hashSet); err != nil {
		terminalErr = fmt.Errorf("snapshot create %s: write manifest: %w: %v",
			snapID, models.ErrSnapshotBackupFailed, err)
		r.failSnap(shareName, snapID, terminalErr)
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

	// --- Step 3: NoVerify short-circuit ---
	if opts.NoVerify {
		// Atomic state+durable flip mirrors the sync-gated path. The
		// remote_durable value is explicit (false) here so the row's
		// durability bit is set in the same UPDATE as the state, not
		// left to the column default. This way the post-create row
		// state is fully deterministic on success and not subject to
		// schema-default drift.
		if err := r.store.MarkSnapshotReady(ctx, shareName, snapID, false, int64(manifestCount)); err != nil {
			terminalErr = fmt.Errorf("snapshot create %s: mark ready (no-verify): %w: %v",
				snapID, models.ErrSnapshotBackupFailed, err)
			r.failSnap(shareName, snapID, terminalErr)
			logger.Error("snapshot create: mark ready failed (no-verify)",
				"snapshot_id", snapID, "share", shareName, "error", err)
			return
		}
		logger.Info("snapshot create: ready (verify gate skipped)",
			"snapshot_id", snapID,
			"share", shareName,
			"final_state", "ready",
			"remote_durable", false,
		)
		return
	}

	// --- Step 4: Verify gate drain ---
	// bs was resolved in Step 0 and reused here.
	logger.Debug("snapshot create: drain start", "snapshot_id", snapID, "share", shareName)
	if err := bs.DrainAllUploads(ctx); err != nil {
		terminalErr = fmt.Errorf("snapshot create %s: drain: %w: %v", snapID, drainSentinel(err), err)
		r.failSnap(shareName, snapID, terminalErr)
		logger.Error("snapshot create: drain failed",
			"snapshot_id", snapID, "share", shareName, "error", err)
		return
	}
	logger.Info("snapshot create: drain complete", "snapshot_id", snapID, "share", shareName)

	// --- Step 5: Verify ---
	remoteStore := bs.RemoteStore()
	if remoteStore == nil {
		terminalErr = fmt.Errorf("snapshot create %s: share %q has no remote store, cannot verify: %w",
			snapID, shareName, models.ErrSnapshotVerifyFailed)
		r.failSnap(shareName, snapID, terminalErr)
		logger.Error("snapshot create: no remote store",
			"snapshot_id", snapID, "share", shareName)
		return
	}
	// --- manifest-completeness guard ---
	// VerifyRemoteDurability only probes the hashes the manifest captured,
	// so it cannot detect a manifest that captured FEWER blocks than the
	// share's metadata still references. Two undercount modes must both be
	// caught here before we can honestly report remote_durable=true:
	//
	//   - empty manifest on a non-empty share (manifestCount == 0 < live):
	//     a rollup that never persisted FileAttr.Blocks, so the verify
	//     would be vacuous over zero blocks.
	//   - partial manifest (0 < manifestCount < live): a backend that
	//     persisted block refs incompletely (the Postgres multi-chunk bug,
	//     #789). The verify would cover only the captured subset and mark
	//     the snapshot durable over a silently-truncated manifest.
	//
	// Enumerate the live hash set UNCONDITIONALLY and require the manifest
	// to cover every block the metadata references (manifestCount ==
	// liveHashes.Len()). A genuinely-empty share (both zero) legitimately
	// passes with a vacuous verify.
	{
		metaStore, mserr := r.GetMetadataStoreForShare(shareName)
		if mserr != nil {
			terminalErr = fmt.Errorf("snapshot create %s: metadata store lookup for manifest-completeness check: %w: %v",
				snapID, models.ErrSnapshotVerifyFailed, mserr)
			r.failSnap(shareName, snapID, terminalErr)
			logger.Error("snapshot create: metadata store lookup failed (manifest-completeness check)",
				"snapshot_id", snapID, "share", shareName, "error", mserr)
			return
		}
		liveHashes, herr := snapshot.HashSetFromMetadataStore(ctx, metaStore)
		if herr != nil {
			terminalErr = fmt.Errorf("snapshot create %s: enumerate live hashes for manifest-completeness check: %w: %v",
				snapID, models.ErrSnapshotVerifyFailed, herr)
			r.failSnap(shareName, snapID, terminalErr)
			logger.Error("snapshot create: live hash enumeration failed (manifest-completeness check)",
				"snapshot_id", snapID, "share", shareName, "error", herr)
			return
		}
		liveCount := liveHashes.Len()
		if manifestCount < liveCount {
			terminalErr = fmt.Errorf("snapshot create %s: manifest covers %d of %d blocks the metadata references, refusing to report durability: %w",
				snapID, manifestCount, liveCount, models.ErrSnapshotManifestIncomplete)
			r.failSnap(shareName, snapID, terminalErr)
			logger.Error("snapshot create: incomplete manifest on non-empty share",
				"snapshot_id", snapID, "share", shareName,
				"manifest_count", manifestCount, "live_hashes", liveCount)
			return
		}
		if liveCount == 0 {
			logger.Info("snapshot create: empty manifest on genuinely-empty share (verify vacuously ok)",
				"snapshot_id", snapID, "share", shareName)
		}
	}

	// Hardcoded; benchmarking confirmed no operator tuning need.
	concurrency := 16
	logger.Debug("snapshot create: verify start",
		"snapshot_id", snapID, "share", shareName,
		"verify_concurrency", concurrency)
	verr := snapshot.VerifyRemoteDurability(ctx, remoteStore, hashSet, concurrency)
	if verr != nil && errors.Is(verr, blockstore.ErrChunkNotFound) {
		// One drain + re-verify retry. Common cause: syncer was behind
		// during the first verify; a fresh drain catches up.
		logger.Debug("snapshot create: verify miss, retrying drain+verify",
			"snapshot_id", snapID, "share", shareName, "first_error", verr)
		if derr := bs.DrainAllUploads(ctx); derr != nil {
			terminalErr = fmt.Errorf("snapshot create %s: re-drain after verify miss: %w: %v",
				snapID, drainSentinel(derr), derr)
			r.failSnap(shareName, snapID, terminalErr)
			logger.Error("snapshot create: re-drain failed",
				"snapshot_id", snapID, "share", shareName, "error", derr)
			return
		}
		verr = snapshot.VerifyRemoteDurability(ctx, remoteStore, hashSet, concurrency)
	}
	if verr != nil {
		terminalErr = fmt.Errorf("snapshot create %s: verify: %w: %v",
			snapID, models.ErrSnapshotVerifyFailed, verr)
		r.failSnap(shareName, snapID, terminalErr)
		logger.Error("snapshot create: verify failed",
			"snapshot_id", snapID, "share", shareName, "error", verr)
		return
	}

	// --- Step 6: Ready flip ---
	// Atomically transition state=creating -> state=ready AND set
	// remote_durable=true in a single conditional UPDATE. A two-step
	// (state, durable) sequence would leave a transient window where a
	// crash mid-update produces ready+remote_durable=false — visually
	// indistinguishable from the intentional --no-verify result and
	// a false negative for restore's durability gate.
	if err := r.store.MarkSnapshotReady(ctx, shareName, snapID, true, int64(manifestCount)); err != nil {
		terminalErr = fmt.Errorf("snapshot create %s: mark ready: %w: %v",
			snapID, models.ErrSnapshotBackupFailed, err)
		r.failSnap(shareName, snapID, terminalErr)
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

// drainSentinel returns the typed sentinel for a DrainAllUploads error:
// ctx cancel / deadline -> ErrSnapshotDrainTimeout, anything else ->
// ErrSnapshotBackupFailed.
func drainSentinel(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return models.ErrSnapshotDrainTimeout
	}
	return models.ErrSnapshotBackupFailed
}

// failSnap flips the snapshot row to state='failed' and persists cause's
// message onto the row's Error column so show/list surface the reason
// instead of "(no error message)". Best-effort: if the row update itself
// fails (e.g., DB unavailable), we log but do not double-fail the
// orchestration error — the wrapped sentinel posted to doneCh is still the
// authoritative signal for callers, and the startup-recovery scan will
// reconcile orphaned creating rows on the next restart.
//
// Uses context.Background so a cancelled parent ctx (the very common
// reason orchestration is bailing out) does not also prevent the failed
// flip.
func (r *Runtime) failSnap(shareName, snapID string, cause error) {
	var msg string
	if cause != nil {
		msg = cause.Error()
	}
	if err := r.store.MarkSnapshotFailed(context.Background(), shareName, snapID, msg); err != nil {
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
// goroutines do not race the per-share snapshots/ tree wipe or any
// in-flight metadata-store I/O.
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
	// Keep the entry visible in the registry while we wait so a concurrent
	// WaitForSnapshot observes the per-snap doneCh and blocks on it,
	// rather than falling through to GetSnapshot and reporting a row in
	// state='creating' with nil orchestration error. Flip draining=true
	// so registerSnapInFlight (if a new CreateSnapshot races in despite
	// the RemoveShare caller's contract) allocates a fresh entry that
	// replaces the map slot — our local entry pointer keeps the wg.Wait
	// pinned to the goroutines actually being drained.
	entry.mu.Lock()
	entry.draining = true
	cancels := make([]context.CancelFunc, 0, len(entry.cancels))
	for _, c := range entry.cancels {
		cancels = append(cancels, c)
	}
	entry.mu.Unlock()
	r.snapInFlightMu.Unlock()

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

	// Now delete the entry — but only if the map slot still references
	// THIS entry. If a new CreateSnapshot raced in and replaced the slot
	// (registerSnapInFlight saw draining=true), we must not clobber the
	// replacement.
	r.snapInFlightMu.Lock()
	if cur, ok := r.snapInFlight[shareName]; ok && cur == entry {
		delete(r.snapInFlight, shareName)
	}
	r.snapInFlightMu.Unlock()

	logger.Info("snapshot lifecycle: in-flight snapshots drained",
		"share", shareName,
	)
}

// shutdownSnapshots cancels all in-flight snapshot goroutines across all
// shares and waits (bounded by ctx) for them to drain. Called as the FIRST
// step of Runtime.Shutdown so snapshot orchestration cannot use-after-close
// the metadata stores or control-plane DB.
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
// start serving, so the partial unique index (one concurrent 'creating'
// row per share) is free for new CreateSnapshot calls.
//
// Recovery is structured-log-only (no schema column): each flip emits a
// slog.Warn marker with reason="abandoned_at_startup" so an operator
// triaging post-crash state can grep the log to distinguish failures
// that happened pre-restart from ones in the current run. The on-disk
// metadata.dump + manifest.hashes are retained (the hold filter
// continues to protect their blocks), and the operator can retry via
// CreateSnapshot with opts.RetryOf set.
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
			if uerr := r.store.MarkSnapshotFailed(ctx, shareName, snap.ID,
				"abandoned: server restarted while snapshot was still creating"); uerr != nil {
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

// RestoreSnapshot synchronously swaps a share's metadata-store contents
// from a previously-created snapshot's dump, gated by pre+post-restore
// remote-block verification and a verified pre-restore safety snapshot.
//
// Caller contract: the share MUST already be disabled (operator runs
// `dfsctl share disable` before invoking restore). Share-disabled is the
// only barrier; no runtime lock is added. On success the share STAYS
// DISABLED — the operator runs `dfsctl share enable` after inspecting
// the restored data.
//
// Orchestration is strictly sequential:
//  1. precheck: share Enabled==false; snapshot in StateReady;
//     RemoteDurable OR opts.AllowNonDurable.
//  2. pre-verify manifest hashes against the remote (no destructive op
//     has run yet).
//  3. create a verified safety snapshot and wait for StateReady.
//  4. open the metadata dump.
//  5. reset the block store's local append-log overlay (BEFORE the
//     metadata Reset, so no concurrent rollup can flush post-snapshot
//     FileBlock rows into the restored metadata).
//  6. Reset the metadata store.
//  7. Restore from the dump.
//  8. post-verify the restored hashes against the remote.
//
// Return values: safetySnapshotID carries the ID of the pre-restore safety
// snapshot. It is empty on precheck or pre-verify failure paths (no safety
// snap was created), and non-empty for every subsequent failure path
// (including success) so callers can present it to the operator for
// rollback.
//
// Failure modes:
//   - Precheck / pre-verify failure: no destructive op ran; no safety snap.
//   - Safety-snap failure: wraps ErrRestoreSafetySnapFailed; no Reset.
//   - Reset / Restore failure: wraps ErrRestoreAborted; safetySnapshotID
//     is set so the operator can roll back.
//   - Post-verify failure: wraps ErrRestoreVerifyFailed; restored metadata
//     is in place but a hash failed to resolve; safetySnapshotID is set.
func (r *Runtime) RestoreSnapshot(
	ctx context.Context,
	shareName, snapID string,
	opts RestoreSnapshotOpts,
) (safetySnapshotID string, err error) {
	if r == nil || r.store == nil {
		return "", errors.New("runtime: nil store")
	}

	// --- precheck ---
	enabled, err := r.sharesSvc.IsShareEnabled(shareName)
	if err != nil {
		return "", err
	}
	if enabled {
		return "", fmt.Errorf("restore snapshot %q on share %q: %w",
			snapID, shareName, models.ErrShareEnabled)
	}

	snap, err := r.store.GetSnapshot(ctx, shareName, snapID)
	if err != nil {
		return "", err
	}
	if snap.State != models.StateReady {
		return "", fmt.Errorf("restore snapshot %q: state=%q, want %q: %w",
			snapID, snap.State, models.StateReady, models.ErrSnapshotStateConflict)
	}
	if !snap.RemoteDurable && !opts.AllowNonDurable {
		return "", fmt.Errorf("restore snapshot %q: %w",
			snapID, models.ErrSnapshotNotDurable)
	}

	localStoreDir, err := r.sharesSvc.LocalStoreDir(shareName)
	if err != nil {
		return "", err
	}

	logger.Info("snapshot restore: precheck ok",
		"snapshot_id", snapID,
		"share", shareName,
		"allow_non_durable", opts.AllowNonDurable,
		"remote_durable", snap.RemoteDurable,
	)

	// --- pre-verify ---
	manifestPath := snap.ManifestPath(localStoreDir)
	manifestFile, err := os.Open(manifestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("restore snapshot %q: open manifest %q: %w: %v",
				snapID, manifestPath, models.ErrSnapshotMetadataDumpMissing, err)
		}
		return "", fmt.Errorf("restore snapshot %q: open manifest %q: %w: %v",
			snapID, manifestPath, models.ErrRestoreVerifyFailed, err)
	}
	manifest, rerr := snapshot.ReadManifest(manifestFile)
	_ = manifestFile.Close()
	if rerr != nil {
		return "", fmt.Errorf("restore snapshot %q: read manifest: %w: %v",
			snapID, models.ErrRestoreVerifyFailed, rerr)
	}

	bs, err := r.sharesSvc.GetBlockStoreForShare(shareName)
	if err != nil {
		return "", fmt.Errorf("restore snapshot %q: block store lookup: %w: %v",
			snapID, models.ErrRestoreVerifyFailed, err)
	}
	if bs == nil {
		return "", fmt.Errorf("restore snapshot %q: share %q has no block store: %w",
			snapID, shareName, models.ErrRestoreVerifyFailed)
	}
	// A share with no remote store is local-only: snapshots there are not
	// remotely durable (the precheck above already required AllowNonDurable
	// for such a snapshot), and there is nothing to HEAD-probe. Skip both
	// the pre- and post-verify remote probes and restore from local CAS.
	// The remote-backed path is unchanged.
	remoteStore := bs.RemoteStore()
	remoteVerify := remoteStore != nil
	// Hardcoded; benchmarking confirmed no operator tuning need.
	concurrency := 16
	if !remoteVerify {
		logger.Info("snapshot restore: local-only share, skipping remote pre-verify",
			"snapshot_id", snapID, "share", shareName)
	} else {
		logger.Info("snapshot restore: pre-verify start",
			"snapshot_id", snapID,
			"share", shareName,
			"manifest_count", manifest.Len(),
			"verify_concurrency", concurrency,
		)
		if verr := snapshot.VerifyRemoteDurability(ctx, remoteStore, manifest, concurrency); verr != nil {
			return "", fmt.Errorf("restore snapshot %q: pre-verify: %w: %v",
				snapID, models.ErrRestoreVerifyFailed, verr)
		}
		logger.Info("snapshot restore: pre-verify ok",
			"snapshot_id", snapID,
			"share", shareName,
		)
	}

	// --- safety snapshot ---
	// Default opts (NoVerify=false) keep the safety snap drained and
	// verified — it is the rollback primitive if any step below fails. On a
	// local-only share there is no remote to verify against, so the safety
	// snap must skip the verify gate too (NoVerify), mirroring the
	// remote-skip applied to the pre/post restore verify above; otherwise
	// the safety snap would fail at its own create-verify step and abort an
	// otherwise-valid local restore.
	safetySnapshotID, err = r.CreateSnapshot(ctx, shareName, CreateSnapshotOpts{NoVerify: !remoteVerify})
	if err != nil {
		return "", fmt.Errorf("restore snapshot %q: create safety snap: %w: %v",
			snapID, models.ErrRestoreSafetySnapFailed, err)
	}
	// Wait on a ctx derived from runtimeCtx, NOT the caller's request ctx:
	// the safety snap's orchestration is itself launched off runtimeCtx
	// (CreateSnapshot), so a client disconnect must not abandon a wait that
	// leaves a stray ready safety snap and a confusingly-aborted restore.
	// The caller's deadline (if any) is preserved so a request timeout still
	// bounds the wait; only the caller's cancellation signal is dropped.
	waitCtx, waitCancel := r.deriveWaitCtx(ctx)
	safetySnap, err := r.WaitForSnapshot(waitCtx, shareName, safetySnapshotID)
	waitCancel()
	if err != nil {
		return safetySnapshotID, fmt.Errorf("restore snapshot %q: wait safety snap %q: %w: %v",
			snapID, safetySnapshotID, models.ErrRestoreSafetySnapFailed, err)
	}
	if safetySnap.State != models.StateReady {
		return safetySnapshotID, fmt.Errorf("restore snapshot %q: safety snap %q final state=%q, want %q: %w",
			snapID, safetySnapshotID, safetySnap.State, models.StateReady, models.ErrRestoreSafetySnapFailed)
	}
	logger.Info("snapshot restore: safety snapshot ready",
		"snapshot_id", snapID,
		"share", shareName,
		"safety_snap_id", safetySnapshotID,
	)

	// --- open dump ---
	dumpPath := snap.MetadataDumpPath(localStoreDir)
	dumpFile, err := os.Open(dumpPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return safetySnapshotID, fmt.Errorf("restore snapshot %q: open dump %q: %w: %v",
				snapID, dumpPath, models.ErrSnapshotMetadataDumpMissing, err)
		}
		return safetySnapshotID, fmt.Errorf("restore snapshot %q: open dump %q: %w: %v",
			snapID, dumpPath, models.ErrRestoreAborted, err)
	}
	defer func() { _ = dumpFile.Close() }()

	// --- reset ---
	metaStore, err := r.GetMetadataStoreForShare(shareName)
	if err != nil {
		return safetySnapshotID, fmt.Errorf("restore snapshot %q: metadata store lookup (safety-snap=%s): %w: %v",
			snapID, safetySnapshotID, models.ErrRestoreAborted, err)
	}
	resetable, ok := metaStore.(metadata.Resetable)
	if !ok {
		return safetySnapshotID, fmt.Errorf("restore snapshot %q: %w",
			snapID, models.ErrMetadataStoreNotResetable)
	}
	backupable, ok := metaStore.(metadata.Backupable)
	if !ok {
		return safetySnapshotID, fmt.Errorf("restore snapshot %q: metadata engine missing Backupable: %w",
			snapID, models.ErrRestoreAborted)
	}
	// --- reset block-store local state (BEFORE the metadata Reset) ---
	// The block store's per-payload append log may still hold post-snapshot
	// write records. ReadPayloadAt replays those records on top of the
	// restored CAS content ("last record wins"), so a file modified in
	// place after the snapshot would come back as the mutated bytes (silent
	// corruption). Dropping the append-log overlay makes the restored CAS
	// manifest the sole source of truth.
	//
	// This MUST run BEFORE resetable.Reset + backupable.Restore (not after):
	// background rollup workers run throughout the restore. If we cleared
	// the overlay only after Restore repopulated the metadata, a rollup
	// worker could call PersistFileBlocks against the freshly-restored
	// metadata in the window between Restore and the clear, injecting
	// post-snapshot FileBlock rows into the restored tree. Clearing the
	// overlay first leaves no dirty intervals for a worker to flush.
	//
	// Safe here because BOTH the snapshot being restored AND the pre-restore
	// safety snapshot drained rollups (CreateSnapshot is synchronous via the
	// WaitForSnapshot above, so the safety snap's DrainRollups completed),
	// so every byte that must survive is already durable in CAS and there
	// are no dirty intervals left to flush.
	if rerr := bs.ResetLocalState(ctx); rerr != nil {
		return safetySnapshotID, fmt.Errorf("restore snapshot %q: reset block-store local state (safety-snap=%s): %w: %v",
			snapID, safetySnapshotID, models.ErrRestoreAborted, rerr)
	}
	logger.Info("snapshot restore: block-store local state reset",
		"snapshot_id", snapID,
		"share", shareName,
		"safety_snap_id", safetySnapshotID,
	)

	logger.Info("snapshot restore: reset start",
		"snapshot_id", snapID,
		"share", shareName,
		"safety_snap_id", safetySnapshotID,
	)
	if err := resetable.Reset(ctx); err != nil {
		return safetySnapshotID, fmt.Errorf("restore snapshot %q: reset (safety-snap=%s): %w: %v",
			snapID, safetySnapshotID, models.ErrRestoreAborted, err)
	}
	logger.Info("snapshot restore: reset ok",
		"snapshot_id", snapID,
		"share", shareName,
		"safety_snap_id", safetySnapshotID,
	)

	// --- restore ---
	logger.Info("snapshot restore: restore start",
		"snapshot_id", snapID,
		"share", shareName,
		"safety_snap_id", safetySnapshotID,
		"dump_path", dumpPath,
	)
	if err := backupable.Restore(ctx, dumpFile); err != nil {
		return safetySnapshotID, fmt.Errorf("restore snapshot %q: restore (safety-snap=%s): %w: %v",
			snapID, safetySnapshotID, models.ErrRestoreAborted, err)
	}
	logger.Info("snapshot restore: restore ok",
		"snapshot_id", snapID,
		"share", shareName,
		"safety_snap_id", safetySnapshotID,
	)

	// --- post-verify ---
	restoredHashes, err := snapshot.HashSetFromMetadataStore(ctx, metaStore)
	if err != nil {
		return safetySnapshotID, fmt.Errorf("restore snapshot %q: enumerate restored hashes (safety-snap=%s): %w: %v",
			snapID, safetySnapshotID, models.ErrRestoreVerifyFailed, err)
	}
	restoredCount := restoredHashes.Len()
	if !remoteVerify {
		logger.Info("snapshot restore: local-only share, skipping remote post-verify",
			"snapshot_id", snapID, "share", shareName, "restored_count", restoredCount)
	} else {
		logger.Info("snapshot restore: post-verify start",
			"snapshot_id", snapID,
			"share", shareName,
			"safety_snap_id", safetySnapshotID,
			"restored_count", restoredCount,
			"verify_concurrency", concurrency,
		)
		if verr := snapshot.VerifyRemoteDurability(ctx, remoteStore, restoredHashes, concurrency); verr != nil {
			return safetySnapshotID, fmt.Errorf("restore snapshot %q: post-verify (safety-snap=%s): %w: %v",
				snapID, safetySnapshotID, models.ErrRestoreVerifyFailed, verr)
		}
	}

	// Share STAYS DISABLED — operator re-enables after inspecting result.
	logger.Info("snapshot restore: complete",
		"snapshot_id", snapID,
		"share", shareName,
		"safety_snap_id", safetySnapshotID,
		"restored_count", restoredCount,
	)
	return safetySnapshotID, nil
}

// GetSnapshot returns the snapshot row for (share, snapID). Delegates to
// the underlying store; ErrSnapshotNotFound propagates verbatim via
// errors.Is.
func (r *Runtime) GetSnapshot(ctx context.Context, share, snapID string) (*models.Snapshot, error) {
	return r.store.GetSnapshot(ctx, share, snapID)
}

// ListSnapshots returns all snapshots for the share ordered newest-first.
// Returns an empty slice (not nil) when no snapshots exist so JSON
// encoding produces [] rather than null.
func (r *Runtime) ListSnapshots(ctx context.Context, share string) ([]*models.Snapshot, error) {
	snaps, err := r.store.ListSnapshots(ctx, share)
	if err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return []*models.Snapshot{}, nil
	}
	return snaps, nil
}

// DeleteSnapshot hard-deletes the snapshot row + its on-disk directory.
//
// Lock ordering: acquires the per-share snapshot delete lock (same mutex
// that gates GC mark-phase reads against deletes) for the full
// store.DeleteSnapshot + os.RemoveAll sequence, then releases. Defense
// in depth: snapID is treated as opaque — any path-separator in it is
// rejected as ErrSnapshotNotFound before any filesystem touch.
//
// Error mapping: ErrSnapshotNotFound from the store propagates verbatim
// (no os.RemoveAll attempted). Other store errors propagate wrapped.
// os.RemoveAll ENOENT is treated as success (idempotent dir wipe);
// other os.RemoveAll errors are logged and wrapped.
func (r *Runtime) DeleteSnapshot(ctx context.Context, share, snapID string) error {
	// Defense-in-depth: snapID is a UUID generated by CreateSnapshot, but
	// reject any caller-supplied value that could escape the share dir.
	if strings.ContainsAny(snapID, "/\\") {
		return models.ErrSnapshotNotFound
	}

	release := r.snapshotDeleteLock(share)
	release.Lock()
	defer release.Unlock()

	// Fence against an in-flight create/retry orchestration for the same
	// snapID: deleting the row + dir out from under a running goroutine
	// would leave it writing dump/manifest into a wiped directory or flip
	// the state of a row that no longer exists. Refuse with a 409-mapped
	// sentinel so the caller retries once the orchestration is terminal.
	if r.isSnapInFlight(share, snapID) {
		return fmt.Errorf("delete snapshot %q on share %q: %w",
			snapID, share, models.ErrSnapshotInFlight)
	}

	if err := r.store.DeleteSnapshot(ctx, share, snapID); err != nil {
		return err
	}

	localStoreDir, err := r.sharesSvc.LocalStoreDir(share)
	if err != nil {
		// The row is gone but we cannot resolve the dir. Memory-only
		// share or share removed between row delete and lookup: nothing
		// to wipe.
		if errors.Is(err, shares.ErrShareNotFound) {
			logger.Info("snapshot deleted (share gone, no dir to wipe)",
				"share", share, "snapshot_id", snapID)
			return nil
		}
		logger.Error("snapshot deleted but local store dir lookup failed",
			"share", share, "snapshot_id", snapID, "err", err)
		return fmt.Errorf("snapshot deleted but local store dir lookup failed: %w", err)
	}
	if localStoreDir == "" {
		// Memory-only share has no on-disk snapshots dir.
		logger.Info("snapshot deleted (memory-only share, no dir to wipe)",
			"share", share, "snapshot_id", snapID)
		return nil
	}
	dir := (&models.Snapshot{ID: snapID}).SnapshotDir(localStoreDir)
	if err := os.RemoveAll(dir); err != nil {
		logger.Error("snapshot dir wipe failed",
			"share", share, "snapshot_id", snapID, "err", err)
		return fmt.Errorf("remove snapshot dir: %w", err)
	}
	logger.Info("snapshot deleted",
		"share", share, "snapshot_id", snapID)
	return nil
}
