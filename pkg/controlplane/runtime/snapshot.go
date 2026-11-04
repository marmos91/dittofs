package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// CreateSnapshotOpts is the operator-facing configuration for one
// CreateSnapshot invocation. Zero-value (NoVerify=false, RetryOf="")
// requests the default behavior: a fresh UUID, verify gate enabled.
type CreateSnapshotOpts struct {
	// Name is an optional human-readable label persisted on the snapshot
	// row. Empty leaves the column empty. Ignored on the RetryOf path,
	// which inherits the original row's Name.
	Name string

	// NoVerify skips the verify gate: its DrainAllUploads and the
	// VerifyRemoteDurability that follows. It does not skip the drain that
	// runs before the metadata dump, which is unconditional because the dump
	// has to carry the block locators that MarkSynced records for carved blocks.
	// So a block store that cannot be reached fails snapshot creation either
	// way, and NoVerify does not make one survivable.
	// Final state: ready with RemoteDurable=false. Restore reads
	// RemoteDurable=false and refuses unless AllowNonDurable is set.
	NoVerify bool

	// RetryOf, when non-empty, reuses the failed snapshot's ID + on-disk
	// dir and atomically overwrites manifest.hashes via
	// WriteManifestAtomic. The target row must currently be in state
	// 'failed' — anything else returns ErrSnapshotRetryTargetNotFailed.
	RetryOf string

	// Scheduled marks the snapshot as produced by the background snapshot
	// scheduler. Only scheduled snapshots are eligible for automatic
	// retention pruning. Ignored on the RetryOf path (the existing row's
	// flag is preserved).
	Scheduled bool

	// AllowDegraded lets the backup complete over metadata rows it cannot
	// read, instead of failing. The result is labelled degraded — on the row
	// and in the snapshot's own directory — so it is never mistaken for a
	// complete one.
	//
	// Off by default and deliberately not exposed as an ordinary operator
	// knob: nobody should get a short snapshot by asking for a snapshot. Its
	// one caller is the safety snapshot restore takes before it resets the
	// store, where refusing blocks recovery from the very corruption that
	// caused the refusal and leaves the share disabled with no undo point.
	AllowDegraded bool
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
//     store dir) OR metadata engine doesn't implement metadata.Snapshotable
//   - models.ErrSnapshotRetryTargetNotFound / models.ErrSnapshotRetryTargetNotFailed
//   - models.ErrSnapshotStateConflict — another in-flight snapshot
//     already exists for this share (partial unique index)
//   - wrapped fs error — snapshot directory could not be created
//
// Goroutine-only failures (observable via WaitForSnapshot):
//   - models.ErrSnapshotBackupFailed — Snapshotable.WriteSnapshot, dump write, or
//     manifest write failed
//   - models.ErrSnapshotDrainTimeout — DrainAllUploads returned a ctx error
//   - models.ErrSnapshotVerifyFailed — sync gate found missing hashes
//     even after one drain retry
//
// On goroutine failure, the row flips to state='failed', metadata.dump
// and manifest.hashes are retained on disk for retry, and the wrapped
// sentinel is posted to the per-snap result chan immediately before close.
func (r *Runtime) CreateSnapshot(ctx context.Context, shareName string, opts CreateSnapshotOpts) (string, error) {
	// A snapshot captures a share's content as well as its metadata, and no
	// content store is wired.
	return "", fmt.Errorf("snapshot create %q: %w", shareName, block.ErrNoContentStore)
}

// WaitForSnapshot returns the snapshot record. No orchestration runs while no
// content store is wired, so the row is the only outcome: a state=failed row
// is rebuilt into its wrapped sentinel via models.SnapshotFailureError, so a
// caller can errors.Is against the typed sentinels. An unknown snapshot id
// answers models.ErrSnapshotNotFound unchanged.
func (r *Runtime) WaitForSnapshot(ctx context.Context, shareName, snapID string) (*models.Snapshot, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("runtime: nil store")
	}
	snap, err := r.store.GetSnapshot(ctx, shareName, snapID)
	if err != nil {
		return nil, err
	}
	if snap.State == models.StateFailed {
		return snap, models.SnapshotFailureError(snap)
	}
	return snap, nil
}

// shutdownSnapshots joins the snapshot scheduler (bounded by ctx) and cancels
// runtimeCtx. Called before the adapters stop and the stores close, so a
// scheduler tick cannot use-after-close the control-plane DB. No snapshot
// orchestration goroutine runs while no content store is wired, so there is
// none to drain.
func (r *Runtime) shutdownSnapshots(ctx context.Context) {
	// Step 0: stop the scheduler that creates the work, and wait for a tick
	// already in flight. It reads and writes snapshot policies through the
	// control-plane store, so it has to be joined here rather than merely
	// signalled — nothing else in the drain waits for it, and the caller
	// closes that store once this returns.
	//
	// decision: the join is bounded by ctx, so on a shutdown whose deadline
	// expires it returns with a tick still running and the caller closes the
	// stores under it. The bound is deliberate — an unbounded join hands one
	// wedged tick the power to stop the process from ever exiting — and the
	// cost of losing the race is an error from a closed store, logged by the
	// tick and discarded, on a process that is milliseconds from exit. What
	// this must not do is claim the guarantee on that path, so it says which
	// one happened. Withdraw the bound if a tick ever performs a write whose
	// partial application outlives the process.

	r.mu.RLock()
	ss := r.snapSchedSvc
	r.mu.RUnlock()
	if ss != nil {
		if !ss.StopContext(ctx) {
			logger.Warn("snapshot drain: scheduler was not joined before the stores close; " +
				"a tick may still be running against them")
		}
	}

	// Cancel every child ctx derived from runtimeCtx. Idempotent.
	if r.runtimeCancel != nil {
		r.runtimeCancel()
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
				"abandoned: server restarted while snapshot was still creating",
				models.FailureKindBackup); uerr != nil {
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

// recoverInterruptedRestores scans the control-plane DB for restore markers
// left behind by a crash that interrupted a RestoreSnapshot, and rolls each
// affected share back to its pre-restore safety snapshot. Called once from
// Runtime.Serve AFTER recoverOrphanedSnapshots (so any safety snapshot
// stranded in state='creating' has already been reconciled to 'failed')
// and BEFORE adapters start serving — a half-restored share must never be
// reachable by clients.
//
// A marker is present iff a restore wrote it (after its safety snap
// verified) but did not reach the post-verify clear. The half-restored
// share could be in any state between "block-store overlay cleared, metadata
// untouched" and "metadata fully replaced but post-verify pending". Rollback
// is the SAME for every case: restore the named safety snapshot (which
// captured the share immediately before the destructive steps), then clear
// the marker. The rollback runs with restoreInternalOpts.isRollback so it
// neither creates a fresh safety snap nor rewrites the marker — the original
// marker is the single source of truth and is cleared only once the rollback
// fully succeeds, so a crash DURING rollback simply re-runs the identical
// rollback on the next boot (idempotent).
//
// Non-fatal: a per-share failure (safety snap gone, reset error) is logged
// and accumulated into firstErr; the scan continues so one unrecoverable
// share does not block startup of the rest. The marker is intentionally
// LEFT in place on rollback failure so a later boot (after the operator
// fixes the cause) retries — clearing it would silently abandon a
// half-restored share.
func (r *Runtime) recoverInterruptedRestores(ctx context.Context) error {
	if r == nil || r.store == nil {
		return nil
	}

	markers, err := r.store.ListRestoreMarkers(ctx)
	if err != nil {
		logger.Error("restore recovery: list restore markers failed", "error", err)
		return err
	}
	if len(markers) == 0 {
		logger.Info("restore recovery: no interrupted restores at startup")
		return nil
	}

	var firstErr error
	var rolledBack, errs int
	for _, m := range markers {
		logger.Warn("restore recovery: interrupted restore detected, rolling back to safety snapshot",
			"share", m.ShareName,
			"target_snapshot_id", m.TargetSnapshotID,
			"safety_snap_id", m.SafetySnapshotID,
			"step_reached", m.Step,
		)

		// Skip-and-clear markers for shares that no longer exist in the
		// runtime registry (the share was removed while a marker was
		// stranded). There is nothing left to roll back, and retaining the
		// marker would log an error on every subsequent boot forever.
		if _, serr := r.sharesSvc.IsShareEnabled(m.ShareName); errors.Is(serr, shares.ErrShareNotFound) {
			logger.Warn("restore recovery: marker for unknown share, clearing (share was removed)",
				"share", m.ShareName,
				"safety_snap_id", m.SafetySnapshotID,
			)
			if derr := r.store.DeleteRestoreMarker(ctx, m.ShareName); derr != nil {
				logger.Error("restore recovery: failed to clear stale marker for unknown share",
					"share", m.ShareName, "error", derr)
				errs++
				if firstErr == nil {
					firstErr = derr
				}
			}
			continue
		}

		// Roll back by restoring the safety snapshot. isRollback suppresses
		// safety-snap creation + marker writes so we don't recurse or grow
		// a pre-restore chain. AllowNonDurable mirrors the original restore's
		// tolerance: the safety snap of a local-only / no-verify share is
		// itself remote_durable=false, and a startup rollback must not refuse
		// on that basis.
		_, rerr := r.restoreSnapshot(ctx, m.ShareName, m.SafetySnapshotID,
			RestoreSnapshotOpts{AllowNonDurable: true},
			restoreInternalOpts{isRollback: true})
		if rerr != nil {
			logger.Error("restore recovery: rollback to safety snapshot failed (marker retained for next boot)",
				"share", m.ShareName,
				"safety_snap_id", m.SafetySnapshotID,
				"error", rerr,
			)
			errs++
			if firstErr == nil {
				firstErr = rerr
			}
			continue
		}

		if derr := r.store.DeleteRestoreMarker(ctx, m.ShareName); derr != nil {
			logger.Error("restore recovery: rollback succeeded but clearing marker failed (may re-trigger harmless rollback on restart)",
				"share", m.ShareName,
				"safety_snap_id", m.SafetySnapshotID,
				"error", derr,
			)
			errs++
			if firstErr == nil {
				firstErr = derr
			}
			continue
		}

		rolledBack++
		logger.Warn("restore recovery: share rolled back to safety snapshot",
			"share", m.ShareName,
			"safety_snap_id", m.SafetySnapshotID,
		)
	}

	logger.Info("restore recovery: complete",
		"markers_found", len(markers),
		"rolled_back", rolledBack,
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
//     FileChunk rows into the restored metadata).
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
	// Inline snapshot metrics: time the operator-initiated restore. The
	// startup-recovery rollback path calls restoreSnapshot directly, so it is
	// (correctly) not counted here as an operator restore.
	opStart := time.Now()
	defer func() {
		result := "ok"
		if err != nil {
			result = "error"
		}
		r.metrics.RecordSnapshotOp("restore", result, time.Since(opStart))
	}()
	return r.restoreSnapshot(ctx, shareName, snapID, opts, restoreInternalOpts{})
}

// restoreInternalOpts carries internal-only restore controls not exposed on
// the public RestoreSnapshotOpts surface.
type restoreInternalOpts struct {
	// isRollback marks this restore as the startup-recovery rollback to a
	// pre-restore safety snapshot. When set, restore skips BOTH the
	// pre-restore safety-snapshot creation AND the durable restore-marker
	// write:
	//   - safety-snap: the half-restored state we are discarding is not
	//     worth capturing (and may fail the create-side verify/empty-manifest
	//     guards), and recursively safety-snapping a rollback would grow an
	//     unbounded chain of pre-restore snaps on every crash-recovery.
	//   - marker: a rollback that is itself interrupted must re-run the SAME
	//     rollback on the next boot (the original marker is only cleared once
	//     the rollback fully completes), so the rollback must not overwrite
	//     that marker with one pointing at a safety snap it never created.
	isRollback bool
}

func (r *Runtime) restoreSnapshot(
	ctx context.Context,
	shareName, snapID string,
	opts RestoreSnapshotOpts,
	internal restoreInternalOpts,
) (safetySnapshotID string, err error) {
	// A restore rewrites a share's content as well as its metadata, and no
	// content store is wired. An interrupted restore's marker is therefore
	// retained until one is.
	return "", fmt.Errorf("restore snapshot %q on share %q: %w", snapID, shareName, block.ErrNoContentStore)
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
// os.RemoveAll + store.DeleteSnapshot sequence, then releases. Defense
// in depth: snapID is treated as opaque — any path-separator in it is
// rejected as ErrSnapshotNotFound before any filesystem touch.
//
// Ordering: the on-disk manifest dir is wiped BEFORE the DB row is
// deleted, so a wipe failure leaves the row intact and the delete stays
// retryable; the only residue the reverse could leave (an orphaned
// manifest dir with no row to retry against) is thereby avoided. A row
// whose dir is already gone is benign — RemoveAll is idempotent on
// ENOENT and the row delete still runs.
//
// Error mapping: a RemoveAll error leaves the row and propagates wrapped.
// ErrSnapshotNotFound from the store delete propagates verbatim after a
// best-effort (idempotent) dir wipe. Other store errors propagate wrapped.
func (r *Runtime) DeleteSnapshot(ctx context.Context, share, snapID string) (err error) {
	// Inline snapshot metrics: record op + duration on every exit, and refresh
	// the held-snapshot count on success.
	opStart := time.Now()
	defer func() {
		result := "ok"
		if err != nil {
			result = "error"
		}
		r.metrics.RecordSnapshotOp("delete", result, time.Since(opStart))
	}()

	// Defense-in-depth: snapID is a UUID generated by CreateSnapshot. Validate
	// it strictly as a UUID before ANY filesystem touch. A separator check
	// alone is insufficient — SnapshotDir filepath.Joins the id, so a value
	// like "." or ".." (no separator) would clean to the snapshots/ parent or
	// the share data dir and let the dir-wipe below RemoveAll the wrong tree.
	if _, perr := uuid.Parse(snapID); perr != nil {
		return models.ErrSnapshotNotFound
	}

	release := r.snapshotDeleteLock(share)
	release.Lock()
	defer release.Unlock()

	// Fence against the per-share restore marker. While a restore is in
	// flight (or crash-interrupted and awaiting startup rollback), the marker
	// names the pre-restore safety snapshot — the SOLE rollback primitive —
	// and the target snapshot it restores from. Deleting either out from
	// under the restore destroys the only recoverable pre-restore state and
	// permanently wedges the share (the next boot's recoverInterruptedRestores
	// cannot find the safety snap, so the marker re-fails every reboot).
	// Runs under the already-held snapshotDeleteLock.
	if marker, merr := r.store.GetRestoreMarker(ctx, share); merr == nil && marker != nil {
		if snapID == marker.SafetySnapshotID || snapID == marker.TargetSnapshotID {
			return fmt.Errorf("delete snapshot %q on share %q: %w",
				snapID, share, models.ErrSnapshotMarkerProtected)
		}
	} else if merr != nil && !errors.Is(merr, models.ErrRestoreMarkerNotFound) {
		// A marker read failure is not authoritative that no restore is in
		// progress; fail closed rather than risk deleting a protected snap.
		return fmt.Errorf("delete snapshot %q on share %q: check restore marker: %w",
			snapID, share, merr)
	}

	// Wipe the on-disk manifest dir BEFORE deleting the DB row. The row is the
	// only durable handle for retrying a delete: if the row went first and the
	// RemoveAll failed, the manifest dir would be orphaned with no way to
	// re-issue DELETE for it. The reverse residue — a row whose dir is already
	// gone — is benign and self-healing: the store delete below removes the
	// row, and even if THAT fails the caller simply retries DELETE (RemoveAll
	// is idempotent on ENOENT).
	//
	// Resolve the share's local store dir first. A memory-only or
	// already-removed share has no dir to wipe; in that case fall straight
	// through to the row delete.
	localStoreDir, err := r.sharesSvc.LocalStoreDir(share)
	switch {
	case errors.Is(err, shares.ErrShareNotFound):
		// Share gone (memory-only or removed): nothing on disk to wipe.
		localStoreDir = ""
	case err != nil:
		logger.Error("snapshot delete: local store dir lookup failed",
			"share", share, "snapshot_id", snapID, "err", err)
		return fmt.Errorf("snapshot delete: local store dir lookup failed: %w", err)
	}

	if localStoreDir != "" {
		dir := (&models.Snapshot{ID: snapID}).SnapshotDir(localStoreDir)
		if err := os.RemoveAll(dir); err != nil {
			// Dir wipe failed: leave the row intact so the operator can
			// retry the whole delete (idempotent) and reach the dir again.
			logger.Error("snapshot dir wipe failed (row left intact for retry)",
				"share", share, "snapshot_id", snapID, "err", err)
			return fmt.Errorf("remove snapshot dir: %w", err)
		}
	}

	if err := r.store.DeleteSnapshot(ctx, share, snapID); err != nil {
		return err
	}

	logger.Info("snapshot deleted",
		"share", share, "snapshot_id", snapID)
	return nil
}
