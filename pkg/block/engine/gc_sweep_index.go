package engine

import (
	"context"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

// sweepFromSyncedIndex is the remote-tier sweep kernel. Instead of LISTing the
// remote namespace (O(total) regardless of churn), it derives the
// remote-orphan candidate set from the synced-hash index: (synced − live).
// Because the carver commits Put-then-record atomically and every delete path
// does reclaim-then-DeleteSynced, the synced set is a strict subset of remote
// contents, so a synced hash that is NOT in the live set (and past grace) is a
// genuine remote orphan.
//
// Post-#1493 every synced hash lives inside a packed blocks/<id> object, so
// reclamation ALWAYS goes through Options.BlockReclaimer: the reclaimer
// decrements the enclosing block and frees the block object + record when its
// last live chunk is gone. There is no per-hash remote object to delete; a
// nil reclaimer, or a hash no share can resolve to a block, is metadata drift
// and is recorded fail-closed (the marker is kept so the hash is re-visited).
//
// Cost = O(synced-set) local scan + O(orphans) block decrements. Orphan block
// objects the index cannot see (a PutBlock-then-commit crash gap) are the
// #1525 reconcile's job (PR5).
//
// Fail-closed rules: a missing first-mirror timestamp (a legacy marker with no
// recorded syncedAt) is preserved, the grace window protects freshly-committed
// hashes, and a live-set hit keeps the marker. The per-object byte size is not
// available from the index, so BytesFreed reflects what the reclaimer reports
// (freed block bytes); ObjectsSwept counts reclaimed chunks.
func sweepFromSyncedIndex(
	ctx context.Context,
	gcs *GCState,
	stats *GCStats,
	snapshotTime time.Time,
	gracePeriod time.Duration,
	dryRunSample int,
	options *Options,
) {
	var statsMu sync.Mutex
	addError := newSweepErrorRecorder(stats, &statsMu)

	graceCutoff := snapshotTime.Add(-gracePeriod)
	var scanned int64

	enumErr := options.SyncedHashIndex.EnumerateSynced(ctx, func(h block.ContentHash, _ block.ChunkLocator, syncedAt time.Time) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := h.String()
		scanned++

		// Fail-closed on a missing first-mirror timestamp: a legacy marker
		// (written before timestamps were stored) cannot be grace-evaluated, so
		// we MUST NOT delete it on the live-set check alone. Preserve it.
		if syncedAt.IsZero() {
			return nil
		}
		// Within the grace window: a freshly-committed hash whose manifest row
		// may not have landed yet — keep it.
		if syncedAt.After(graceCutoff) {
			return nil
		}

		present, err := gcs.Has(h)
		if err != nil {
			// Fail-closed: cannot prove the hash is orphaned, so keep it.
			addError("gcstate has " + key + ": " + err.Error())
			return nil
		}
		if present {
			return nil // live (manifest or snapshot-held) — keep
		}

		if options.DryRun {
			recordDryRunCandidate(stats, &statsMu, key, dryRunSample)
			return nil
		}

		// Block reclaim (#1414 object packing) is the ONLY remote reclaim
		// path: the sweep reaches h only here, where it has already proven h
		// globally dead (past grace, absent from the live set), so
		// decrementing the enclosing block can never race a live dedup
		// sibling. A deployment without a reclaimer cannot reclaim anything —
		// record the drift and keep the marker (fail-closed).
		if options.BlockReclaimer == nil {
			addError("block-reclaim " + key + ": no block reclaimer wired — dead chunk kept (drift)")
			return nil
		}
		handled, freed, rerr := options.BlockReclaimer.ReclaimDeadChunk(ctx, h)
		if rerr != nil {
			addError("block-reclaim " + key + ": " + rerr.Error())
			return nil
		}
		if !handled {
			// Post-migration every synced hash must resolve to a block
			// locator in some share. A hash no share can resolve is metadata
			// drift: keep the marker (never guess at a remote key) and
			// surface it.
			addError("block-reclaim " + key + ": no share resolves a block locator — dead chunk kept (drift)")
			return nil
		}
		// Clear the synced marker (its locator was already consumed by the
		// reclaim) so the synced set stays a strict subset of remote contents
		// (#1433). Only count the reclamation when the marker is actually
		// cleared — a surviving marker means the hash will be re-visited next
		// pass, so counting it now would double-count both ObjectsSwept and
		// BytesFreed.
		if serr := options.SyncedHashIndex.DeleteSynced(ctx, h); serr != nil {
			addError("delete-synced " + key + ": " + serr.Error())
			return nil // marker survives; retry next pass, don't count yet
		}
		statsMu.Lock()
		stats.ObjectsSwept++
		stats.BytesFreed += freed
		statsMu.Unlock()
		return nil
	})
	if enumErr != nil {
		addError("enumerate-synced: " + enumErr.Error())
	}

	statsMu.Lock()
	// ObjectsScanned here counts synced markers inspected (the LIST-free
	// candidate set), not the full remote namespace — the index IS the scope.
	stats.ObjectsScanned += scanned
	statsMu.Unlock()

	if options.ProgressCallback != nil {
		statsMu.Lock()
		snap := *stats
		statsMu.Unlock()
		options.ProgressCallback(snap)
	}
}
