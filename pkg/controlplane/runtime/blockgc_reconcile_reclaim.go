package runtime

import (
	"context"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/remote"
)

// ReconcileReclaim reclaims orphaned block storage server-wide — the deleting
// stages after the read-only reporter (PR5a, #1493/#1525):
//
//   - class 1 (zero-ref records) and class 2 (leaked records, #1525): block
//     records with no live locator, deleted along with their remote object.
//   - class 3 (record-less remote objects): blocks/<id> objects with no backing
//     record, older than the grace window, deleted from the bucket.
//
// dryRun tallies what would be reclaimed without deleting.
//
// Like the GC sweep it unions the shares that reference each remote and holds the
// same per-remote GC lock while reclaiming, so a delete can never race a concurrent
// sweep's check-then-DecrLiveChunkCount on the same shared block.
//
// Class 3 is fail-closed: its "has a record" set (metaBlockIDs) is unioned across
// EVERY share on the remote, and the object sweep is skipped for a remote unless
// all of its shares were fully enumerated — otherwise a sibling share's live object
// would be misread as an orphan and deleted. A read-only backend (records
// enumerable but not deletable) still contributes to the union and keeps class 3
// safe; only its class-1/2 records are left unreclaimed.
func (r *Runtime) ReconcileReclaim(ctx context.Context, dryRun bool) (*engine.ReclaimReport, error) {
	grace := r.reconcileGracePeriod()
	total := &engine.ReclaimReport{}
	// remoteBytes/liveLogical accumulate across every remote-backed share
	// visited this sweep, for the space-amplification log below.
	var remoteBytes, liveLogical int64
	for _, entry := range r.sharesSvc.DistinctRemoteStores() {
		if err := ctx.Err(); err != nil {
			return total, err
		}

		// views: shares whose records we can delete (class 1/2).
		// metaBlockIDs: every block ID with a record, across ALL shares on this
		// remote (class-3 safety set).
		// countedBlockIDs: subset of metaBlockIDs already folded into
		// remoteBytes, so a block visible from two shares that share a
		// metadata store instance isn't double-counted there.
		// allEnumerated: false if any share could not be fully walked, which
		// makes the class-3 union incomplete and unsafe to act on.
		views := make([]engine.ReclaimMetaView, 0, len(entry.Shares))
		metaBlockIDs := make(map[string]struct{})
		countedBlockIDs := make(map[string]struct{})
		allEnumerated := true
		for _, shareName := range entry.Shares {
			mds, err := r.GetMetadataStoreForShare(shareName)
			if err != nil {
				logger.Warn("ReconcileReclaim: metadata store unavailable — class-3 sweep disabled for this remote",
					"share", shareName, "err", err)
				allEnumerated = false
				continue
			}
			rv, ok := mds.(engine.ReconcileMetaView)
			if !ok {
				logger.Warn("ReconcileReclaim: metadata store does not support reconcile — class-3 sweep disabled for this remote",
					"share", shareName)
				allEnumerated = false
				continue
			}
			// Buffered per share so a walk that fails partway (line below)
			// discards this share's bytes instead of leaving remoteBytes
			// holding a partial sum while liveLogical already counts the
			// share in full — a share contributes to both totals or to
			// neither. The two still measure different things: remoteBytes
			// counts each block once across all shares, while liveLogical
			// sums per-share usage, so shares referencing a shared block
			// each count its logical bytes. The ratio is an estimate.
			shareBlocks := make(map[string]int64)
			walkErr := rv.WalkBlockRecords(ctx, func(rec block.BlockRecord) error {
				metaBlockIDs[rec.BlockID] = struct{}{}
				shareBlocks[rec.BlockID] = rec.Length
				return nil
			})
			if walkErr != nil {
				logger.Warn("ReconcileReclaim: walk block records failed — class-3 sweep disabled for this remote",
					"share", shareName, "err", walkErr)
				allEnumerated = false
			} else if used, usedErr := mds.GetUsedBytesForShare(ctx, shareName); usedErr == nil {
				for id, length := range shareBlocks {
					if _, counted := countedBlockIDs[id]; !counted {
						countedBlockIDs[id] = struct{}{}
						remoteBytes += length
					}
				}
				liveLogical += used
			} else {
				// The share is left out of both totals, so the ratio stays
				// coherent over the shares that did report. The block walk
				// succeeded, so metaBlockIDs is still complete and the
				// class-3 sweep stays safe to act on.
				logger.Warn("ReconcileReclaim: share usage lookup failed — share left out of the space amplification estimate",
					"share", shareName, "err", usedErr)
			}
			// DeleteBlockRecord is the extra method the reclaimer needs beyond the
			// read-only view; a backend lacking it still contributes to the union
			// above but its records cannot be reclaimed.
			if wv, ok := mds.(engine.ReclaimMetaView); ok {
				views = append(views, wv)
			}
		}

		rbs, _ := entry.Store.(remote.RemoteBlockStore)
		opts := engine.ReclaimOptions{DryRun: dryRun, GracePeriod: grace}

		var rep engine.ReclaimReport
		err := func() error {
			lock := r.remoteGCLock(entry.ConfigID)
			lock.Lock()
			defer lock.Unlock()

			var e error
			if rep, e = engine.ReclaimRecords(ctx, views, rbs, opts); e != nil {
				return e
			}
			if rbs == nil {
				return nil // no remote store — nothing to sweep for class 3
			}
			// Class 3 only when every share was enumerable, else the union is
			// incomplete and a live sibling object could be deleted.
			if !allEnumerated {
				logger.Warn("ReconcileReclaim: class-3 orphan-object sweep skipped — not all shares on remote were enumerable",
					"configID", entry.ConfigID)
				return nil
			}
			orep, e := engine.ReclaimOrphanObjects(ctx, metaBlockIDs, rbs, opts)
			if e != nil {
				return e
			}
			rep.Merge(orep)
			return nil
		}()
		if err != nil {
			return total, err
		}
		total.Merge(rep)
	}

	logger.Info("ReconcileReclaim: complete",
		"zeroRefReclaimed", total.Reclaimed.Count,
		"leakedReclaimed", total.LeakedReclaimed.Count,
		"orphanObjectsReclaimed", total.OrphanObjectsReclaimed.Count,
		"bytesFreed", total.Reclaimed.Bytes+total.LeakedReclaimed.Bytes+total.OrphanObjectsReclaimed.Bytes,
		"blockRecordsScanned", total.BlockRecordsScanned,
		"remoteObjectsScanned", total.RemoteObjectsScanned,
		"errors", total.Errors,
		"dryRun", total.DryRun,
	)

	// ponytail: a scattered carve now packs ~1000 chunks into one block, and a
	// block is reclaimed only when its last member dies — under random overwrite
	// that is effectively never, so remote space tracks bytes written rather than
	// bytes live. This ratio is the trigger: upgrade to a low-liveness repack
	// (block-level sync state) when it stays above ~2.0 in the field.
	if liveLogical > 0 {
		logger.Info("block gc sweep: remote space amplification",
			"remote_bytes", remoteBytes,
			"live_logical_bytes", liveLogical,
			"ratio", float64(remoteBytes)/float64(liveLogical))
	}

	return total, nil
}
