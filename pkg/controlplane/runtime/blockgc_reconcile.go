package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	blockgc "github.com/marmos91/dittofs/pkg/block/gc"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// reconcileMarkerKey names the per-store CustomSettings entry recording that the
// one-time stranded-row reconcile has run. Bump reconcileVersion to force a
// re-run on upgrade if a future fix needs another pass.
const (
	reconcileMarkerKey = "gc.stranded_rows_reconciled_version"
	// v2 (#1433): EnumerateLivePayloadIDs now excludes nlink=0 inodes, so the
	// reconcile reaps stranded file_blocks rows left by pre-fix unlinks. Bumping
	// the version forces one more startup pass on stores last reconciled at v1.
	reconcileVersion = 2
)

// RunBlockGCReconcile reaps stranded file_blocks rows — rows whose owning inode
// is already gone — and then runs the normal two-tier sweep so the now-orphaned
// chunks are reclaimed on both local and remote stores.
//
// This is the migration path for blocks leaked by the pre-fix delete path
// (#1433): a plain GC pass cannot reclaim them, because a stranded row keeps its
// hash in the live set (the live set is built from file_blocks, which has no
// namespace join). The reconcile recomputes the true live set from the inode
// namespace (EnumerateLivePayloadIDs), reaps the rows the namespace no longer
// references, then lets the grace- and snapshot-hold-aware sweep delete the
// chunks.
func (r *Runtime) RunBlockGCReconcile(ctx context.Context, dryRun bool) (*blockgc.GCStats, error) {
	stats, _, err := r.runBlockGCReconcile(ctx, dryRun, nil)
	return stats, err
}

// runBlockGCReconcile is RunBlockGCReconcile with an optional progress sink
// wired into the sweep for async callers (StartBlockGC). progress is nil for
// the synchronous public entrypoint and the startup reconcile-once.
//
// It also reports the shares whose reap did NOT complete. A per-share failure
// is logged and skipped rather than aborting the run — one broken share must
// not stop the others — but the run is then not a reconcile of that share, and
// the caller that persists the once-per-store marker has to know which ones to
// leave unmarked, or the migration is recorded as done having never happened.
func (r *Runtime) runBlockGCReconcile(
	ctx context.Context,
	dryRun bool,
	progress func(blockgc.GCStats),
) (*blockgc.GCStats, map[string]struct{}, error) {
	graceCutoff := time.Now().Add(-r.reconcileGracePeriod())

	total := &blockgc.GCStats{DryRun: dryRun}
	failed := make(map[string]struct{})
	for _, shareName := range r.ListShares() {
		if err := ctx.Err(); err != nil {
			return total, failed, err
		}
		mds, err := r.GetMetadataStoreForShare(shareName)
		if err != nil {
			logger.Warn("RunBlockGCReconcile: get metadata store", "share", shareName, "err", err)
			total.ErrorCount++
			failed[shareName] = struct{}{}
			continue
		}
		reaped, err := r.reapStrandedRows(ctx, shareName, mds, graceCutoff, dryRun)
		if err != nil {
			logger.Error("RunBlockGCReconcile: reap stranded rows", "share", shareName, "err", err)
			total.ErrorCount++
			failed[shareName] = struct{}{}
			continue
		}
		total.StrandedRowsReaped += int64(reaped)
	}

	// Stranded rows are gone, so their hashes have left the live set. The sweep
	// reclaims the now-orphaned chunks on the remote tier and (since #1433) the
	// local tier too, under the usual grace + snapshot-hold guards. The remote
	// tier sweeps from the synced-hash index; orphan block objects the index
	// cannot see (a PutBlock-then-commit crash gap) are the #1525 reconcile
	// sweep's job (PR5).
	sweep, err := r.runBlockGCSweep(ctx, dryRun, progress)
	// Record reaped rows regardless of the sweep result: the rows are already
	// deleted from the metadata store, so the counter must reflect that even if
	// the downstream sweep (e.g. S3 unreachable) then fails. Skip on dry-run —
	// there the count is rows that *would* be reaped, nothing was deleted.
	if !dryRun {
		r.metrics.RecordGCStrandedRows(total.StrandedRowsReaped)
	}
	if err != nil {
		return total, failed, err
	}
	accumulateGCStats(total, sweep)
	logger.Info("RunBlockGCReconcile: complete",
		"dryRun", dryRun,
		"strandedRowsReaped", total.StrandedRowsReaped,
		"objectsSwept", total.ObjectsSwept,
		"bytesFreed", total.BytesFreed,
		"errors", total.ErrorCount,
	)
	return total, failed, nil
}

// reconcileGracePeriod returns the configured GC grace period (default 1h). The
// reconcile uses it to skip rows created within the window, closing the TOCTOU
// race where a file is created between the live-set snapshot and the stranded
// scan.
func (r *Runtime) reconcileGracePeriod() time.Duration {
	if d := r.gcDefaultsSnapshot(); d != nil && d.GracePeriod > 0 {
		return d.GracePeriod
	}
	return time.Hour
}

// reapStrandedRows reaps file_blocks rows for one share whose payloadID is not
// referenced by any live inode. Rows newer than graceCutoff are skipped (a file
// may have been created mid-scan). When dryRun is set, rows are counted but not
// reaped.
//
// This runs with dryRun=false from the startup reconcile, in a goroutine
// detached from shutdown, so every guard below is the only thing between a
// misread of the namespace and deleted rows. It reaps the set difference, which
// means it deletes precisely what the live set omits — so the live set must be
// COMPLETE, not merely usable.
func (r *Runtime) reapStrandedRows(ctx context.Context, shareName string, mds metadata.Store, graceCutoff time.Time, dryRun bool) (int, error) {
	// True live set, from the namespace. Fail closed on ErrLiveSetIncomplete:
	// the scan is deliberately best-effort and skips a namespace row it cannot
	// decode, which is right for a reclaim and wrong here. Every payload owned
	// by a skipped inode is missing from `live`, looks stranded, and would be
	// reaped — the scan's own tolerance turned into data loss. A partial set
	// cannot authorize a deletion, so the whole share is left alone; the caller
	// records it as unreconciled, so the migration marker is not persisted for
	// it and the next start tries again.
	live := make(map[string]struct{})
	if err := mds.EnumerateLivePayloadIDs(ctx, func(p string) error {
		live[p] = struct{}{}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("enumerate live payloads: %w", err)
	}

	// Payloads present in file_blocks but absent from the namespace = stranded.
	var stranded []string
	if err := mds.EnumeratePayloads(ctx, func(p string) error {
		if _, ok := live[p]; !ok {
			stranded = append(stranded, p)
		}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("enumerate payloads: %w", err)
	}
	if len(stranded) == 0 {
		return 0, nil
	}

	// Open-handle hold (#1448): a payload referenced by an open-but-unlinked
	// file is not stranded — the open handle still reads through its rows, and
	// reaping them would both destroy the manifest and drop the hashes from
	// the mark live set. Fail closed: if the held set cannot be resolved, the
	// reconcile must not reap.
	heldOpen, err := r.openPayloadIDsForShare(ctx, shareName)
	if err != nil {
		return 0, fmt.Errorf("resolve open-handle held payloads: %w", err)
	}

	reaped, skippedGrace, skippedOpen := 0, 0, 0
	for _, pid := range stranded {
		if err := ctx.Err(); err != nil {
			return reaped, err
		}
		if _, ok := heldOpen[pid]; ok {
			skippedOpen++
			continue
		}
		rows, err := mds.ListFileChunks(ctx, pid)
		if err != nil {
			logger.Warn("reconcile: list stranded blocks", "share", shareName, "payload", pid, "err", err)
			continue
		}
		for _, fb := range rows {
			if fb == nil {
				continue
			}
			if fb.CreatedAt.After(graceCutoff) {
				skippedGrace++
				continue
			}
			if dryRun {
				reaped++
				continue
			}
			if _, err := mds.DecrementRefCountAndReap(ctx, fb.ID); err != nil {
				logger.Warn("reconcile: reap row", "share", shareName, "id", fb.ID, "err", err)
				continue
			}
			reaped++
		}
	}
	logger.Info("reconcile: stranded rows processed",
		"share", shareName,
		"strandedPayloads", len(stranded),
		"rowsReaped", reaped,
		"skippedGrace", skippedGrace,
		"skippedOpenHandle", skippedOpen,
		"dryRun", dryRun,
	)
	return reaped, nil
}

// RunBlockGCReconcileOnce runs the stranded-row reconcile exactly once per
// metadata store, guarded by a persisted marker, then chains the sweep. It is
// the upgrade migration: existing deployments leaked blocks before the
// unlink-refcount fix, and this reclaims them without operator action. Safe to
// call on every startup — it no-ops once the marker is set. Intended to run in a
// detached goroutine; errors are logged, never fatal.
func (r *Runtime) RunBlockGCReconcileOnce(ctx context.Context) {
	shares := r.ListShares()
	pending := make([]string, 0, len(shares))
	for _, shareName := range shares {
		mds, err := r.GetMetadataStoreForShare(shareName)
		if err != nil {
			logger.Warn("reconcile-once: get metadata store", "share", shareName, "err", err)
			continue
		}
		if reconcileAlreadyDone(ctx, mds) {
			continue
		}
		pending = append(pending, shareName)
	}
	if len(pending) == 0 {
		logger.Debug("reconcile-once: all stores already reconciled; skipping")
		return
	}

	logger.Info("reconcile-once: running one-time stranded-row migration", "shares", pending)
	_, failed, err := r.runBlockGCReconcile(ctx, false, nil)
	if err != nil {
		logger.Error("reconcile-once: reconcile failed; marker not set, will retry next start", "err", err)
		return
	}

	// Persist the marker only on the stores that actually got reconciled. A
	// share whose reap refused — an incomplete live set, an unreachable store —
	// was skipped, not reconciled, and marking it would retire the migration for
	// good: reconcileAlreadyDone no-ops every subsequent start, so its stranded
	// rows are never reclaimed, silently, with no way back short of bumping
	// reconcileVersion.
	for _, shareName := range pending {
		if _, bad := failed[shareName]; bad {
			logger.Warn("reconcile-once: share not reconciled; marker withheld, will retry next start",
				"share", shareName)
			continue
		}
		mds, err := r.GetMetadataStoreForShare(shareName)
		if err != nil {
			continue
		}
		if err := markReconcileDone(ctx, mds); err != nil {
			logger.Warn("reconcile-once: persist marker", "share", shareName, "err", err)
		}
	}
}

// reconcileAlreadyDone reports whether the store's marker is at or above the
// current reconcile version. JSON round-trips numbers as float64, so the
// assertion must be float64.
func reconcileAlreadyDone(ctx context.Context, mds metadata.Store) bool {
	cfg, err := mds.GetServerConfig(ctx)
	if err != nil || cfg.CustomSettings == nil {
		return false
	}
	switch v := cfg.CustomSettings[reconcileMarkerKey].(type) {
	case float64:
		return v >= reconcileVersion
	case int:
		return v >= reconcileVersion
	default:
		return false
	}
}

// markReconcileDone records the current reconcile version in the store's
// CustomSettings so RunBlockGCReconcileOnce no-ops on subsequent starts.
func markReconcileDone(ctx context.Context, mds metadata.Store) error {
	cfg, err := mds.GetServerConfig(ctx)
	if err != nil {
		return err
	}
	if cfg.CustomSettings == nil {
		cfg.CustomSettings = make(map[string]any)
	}
	cfg.CustomSettings[reconcileMarkerKey] = reconcileVersion
	return mds.SetServerConfig(ctx, cfg)
}
