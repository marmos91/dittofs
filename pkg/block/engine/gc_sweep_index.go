package engine

import (
	"context"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

// sweepFromSyncedIndex is the index-based counterpart of sweepByWalk. Instead
// of a full RemoteStore.Walk (an S3 LIST of the entire CAS namespace, O(total)
// regardless of churn), it derives the remote-orphan candidate set from the
// synced-hash index: (synced − live). Because the mirror does Put-then-Mark and
// every delete path does Delete-then-DeleteSynced, the synced set is a strict
// subset of remote contents, so a synced hash that is NOT in the live set (and
// past grace) is a genuine remote orphan that can be deleted by key — no LIST.
//
// Cost = O(synced-set) local scan + O(orphans) DELETEs. Rare index drift
// (e.g. a Put-then-Mark crash leaving an un-indexed remote object) is caught by
// the periodic full-Walk reconcile, not here.
//
// Semantics match sweepByWalk where they overlap: fail-closed on a missing
// timestamp (a legacy marker with no recorded syncedAt is preserved), the grace
// window protects freshly-mirrored hashes, and a live-set hit keeps the object.
// Unlike the Walk sweep, the per-object byte size is not available from the
// index, so BytesFreed is not tracked here — ObjectsSwept is the accurate
// reclamation count; the periodic full-Walk reconcile reports bytes.
func sweepFromSyncedIndex(
	ctx context.Context,
	store sweepable,
	gcs *GCState,
	stats *GCStats,
	snapshotTime time.Time,
	gracePeriod time.Duration,
	dryRunSample int,
	options *Options,
) {
	var statsMu sync.Mutex

	seenClasses := make(map[string]struct{}, 16)
	addError := func(msg string) {
		statsMu.Lock()
		defer statsMu.Unlock()
		stats.ErrorCount++
		cls := classifyGCError(msg)
		if _, ok := seenClasses[cls]; ok {
			return
		}
		if len(seenClasses) >= 16 {
			return
		}
		seenClasses[cls] = struct{}{}
		stats.FirstErrors = append(stats.FirstErrors, msg)
	}

	graceCutoff := snapshotTime.Add(-gracePeriod)
	var scanned int64

	enumErr := options.SyncedHashIndex.EnumerateSynced(ctx, func(h block.ContentHash, syncedAt time.Time) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		casKey := block.FormatCASKey(h)
		scanned++

		// Fail-closed on a missing first-mirror timestamp: a legacy marker
		// (written before timestamps were stored) cannot be grace-evaluated, so
		// we MUST NOT delete it on the live-set check alone. Preserve it; the
		// periodic full-Walk reconcile will reclaim it if it is a true orphan.
		if syncedAt.IsZero() {
			return nil
		}
		// Within the grace window: a freshly-mirrored hash whose manifest row
		// may not have committed yet — keep it.
		if syncedAt.After(graceCutoff) {
			return nil
		}

		present, err := gcs.Has(h)
		if err != nil {
			// Fail-closed: cannot prove the hash is orphaned, so keep it.
			addError("gcstate has " + casKey + ": " + err.Error())
			return nil
		}
		if present {
			return nil // live (manifest or snapshot-held) — keep
		}

		if options.DryRun {
			statsMu.Lock()
			if int64(len(stats.DryRunCandidates)) < int64(dryRunSample) {
				stats.DryRunCandidates = append(stats.DryRunCandidates, casKey)
			}
			stats.ObjectsSwept++ // count what would be deleted
			statsMu.Unlock()
			return nil
		}

		if derr := store.Delete(ctx, h); derr != nil {
			// Continue + capture; the marker stays so the next pass retries.
			addError("delete " + casKey + ": " + derr.Error())
			return nil
		}
		// Keep the synced index a strict subset of remote contents: the object
		// is gone, so clear its marker. Idempotent + non-fatal — a stale marker
		// only costs a missed re-upload, recovered on the next pass (#1433).
		if serr := options.SyncedHashIndex.DeleteSynced(ctx, h); serr != nil {
			addError("delete-synced " + casKey + ": " + serr.Error())
		}
		statsMu.Lock()
		stats.ObjectsSwept++
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
