package runtime

import (
	"context"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/remote"
)

// ReconcileReclaimZeroRef reclaims class-1 orphans server-wide: block records
// with a zero live chunk count and no live locator — a crash between
// DecrLiveChunkCount and DeleteBlockRecord. It frees each record's remote block
// object (idempotent) and deletes the record. This is PR5b, the first DELETING
// reconcile stage after the read-only reporter (PR5a, #1493/#1525); dryRun
// tallies what would be reclaimed without deleting.
//
// Like the GC sweep it unions the shares that reference each remote and holds the
// same per-remote GC lock while reclaiming, so a zero-ref delete can never race a
// concurrent sweep's check-then-DecrLiveChunkCount on the same shared block.
func (r *Runtime) ReconcileReclaimZeroRef(ctx context.Context, dryRun bool) (*engine.ReclaimReport, error) {
	total := &engine.ReclaimReport{}
	for _, entry := range r.sharesSvc.DistinctRemoteStores() {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		views := make([]engine.ReclaimMetaView, 0, len(entry.Shares))
		for _, shareName := range entry.Shares {
			mds, err := r.GetMetadataStoreForShare(shareName)
			if err != nil {
				logger.Warn("ReconcileReclaimZeroRef: metadata store unavailable — share excluded",
					"share", shareName, "err", err)
				continue
			}
			// EnumerateSynced/DeleteBlockRecord are required to re-derive the
			// live set and delete; a backend lacking them cannot be reclaimed
			// safely, so exclude it rather than risk a false delete.
			view, ok := mds.(engine.ReclaimMetaView)
			if !ok {
				logger.Warn("ReconcileReclaimZeroRef: metadata store does not support reclaim — share excluded",
					"share", shareName)
				continue
			}
			views = append(views, view)
		}
		if len(views) == 0 {
			continue
		}
		// A remote that cannot hold packed blocks (no RemoteBlockStore) still has
		// its records reclaimed; there is simply nothing to free remotely.
		rbs, _ := entry.Store.(remote.RemoteBlockStore)

		var rep engine.ReclaimReport
		err := func() error {
			lock := r.remoteGCLock(entry.ConfigID)
			lock.Lock()
			defer lock.Unlock()
			var e error
			rep, e = engine.ReclaimZeroRefRecords(ctx, views, rbs, engine.ReclaimOptions{DryRun: dryRun})
			return e
		}()
		if err != nil {
			return total, err
		}
		total.Merge(rep)
	}

	logger.Info("ReconcileReclaimZeroRef: complete",
		"reclaimed", total.Reclaimed.Count,
		"bytesFreed", total.Reclaimed.Bytes,
		"blockRecordsScanned", total.BlockRecordsScanned,
		"errors", total.Errors,
		"dryRun", total.DryRun,
	)
	return total, nil
}
