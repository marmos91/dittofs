package runtime

import (
	"context"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/remote"
)

// ReconcileReport scans every remote-backed share for orphaned block storage
// and returns a structured, READ-ONLY report of the four orphan classes
// (#1493/#1525 reconcile reporter, PR5a): zero-ref records, leaked blocks,
// record-less remote objects (past the grace window), and stranded local-only
// chunks. It mutates nothing — no deletes, no decrements, no marker changes —
// so an operator can review orphans before the later delete stages act.
//
// It is server-wide: for each remote store it unions the shares that reference
// it (mirroring the GC per-remote reconciler) so a sibling share's live block
// is never misreported as a record-less object. Per-remote reports are folded
// into one aggregate.
func (r *Runtime) ReconcileReport(ctx context.Context) (*engine.ReconcileReport, error) {
	grace := r.reconcileGracePeriod()

	// Per-share local views (class 4), indexed by share. Only stores exposing
	// ListUnsynced (a durable local tier) contribute; in-memory backends have
	// nothing to strand.
	localsByShare := make(map[string][]engine.ReconcileLocalView)
	for _, le := range r.sharesSvc.ShareLocalStores() {
		if lv, ok := le.Store.(engine.ReconcileLocalView); ok {
			localsByShare[le.ShareName] = append(localsByShare[le.ShareName], lv)
		}
	}

	total := &engine.ReconcileReport{}
	for _, entry := range r.sharesSvc.DistinctRemoteStores() {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		views := make([]engine.ReconcileMetaView, 0, len(entry.Shares))
		var locals []engine.ReconcileLocalView
		for _, shareName := range entry.Shares {
			mds, err := r.GetMetadataStoreForShare(shareName)
			if err != nil {
				logger.Warn("ReconcileReport: metadata store unavailable — share excluded from scan",
					"share", shareName, "err", err)
				continue
			}
			// EnumerateSynced is an off-interface concrete method; a backend
			// that lacks it cannot supply the live locator set, so its records
			// would be misreported as orphans. Exclude it rather than misreport.
			view, ok := mds.(engine.ReconcileMetaView)
			if !ok {
				logger.Warn("ReconcileReport: metadata store does not support reconcile scan — share excluded",
					"share", shareName)
				continue
			}
			views = append(views, view)
			locals = append(locals, localsByShare[shareName]...)
		}
		// No usable metadata view for any share on this remote: metaBlockIDs
		// would be empty, so EVERY aged remote object would be misreported as a
		// record-less orphan (class 3). Skip the remote fail-closed rather than
		// emit false orphans. Unreachable with all current backends (each
		// implements ReconcileMetaView).
		if len(views) == 0 {
			logger.Warn("ReconcileReport: no metadata views for remote — scan skipped fail-closed",
				"configID", entry.ConfigID, "shares", entry.Shares)
			continue
		}
		// A remote that cannot hold packed blocks (no RemoteBlockStore) still
		// gets classes 1/2 scanned; class 3 is skipped with a nil remote.
		rbs, _ := entry.Store.(remote.RemoteBlockStore)
		rep, err := engine.Reconcile(ctx, views, rbs, locals, engine.ReconcileOptions{GracePeriod: grace})
		if err != nil {
			return total, err
		}
		total.Merge(rep)
	}

	logger.Info("ReconcileReport: complete",
		"zeroRefRecords", total.ZeroRefRecords.Count,
		"leakedBlocks", total.LeakedBlocks.Count,
		"orphanRemoteObjects", total.OrphanRemoteObjects.Count,
		"strandedLocalChunks", total.StrandedLocalChunks.Count,
		"blockRecordsScanned", total.BlockRecordsScanned,
		"remoteObjectsScanned", total.RemoteObjectsScanned,
		"gracePeriod", total.GracePeriod,
	)
	return total, nil
}
