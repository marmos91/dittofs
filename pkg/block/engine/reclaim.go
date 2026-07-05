// Package engine — orphan-storage reclaim (#1493/#1525 reconcile, PR5b+PR5c), the
// *deleting* stages after the read-only reporter (reconcile.go). Both operate on a
// record's live-locator status, never its (possibly stale) LiveChunkCount:
//
//   - class 1 (ReclaimRecords, zero-ref): record with no live locator and
//     LiveChunkCount == 0 — a crash between DecrLiveChunkCount and
//     DeleteBlockRecord.
//   - class 2 (ReclaimRecords, leaked): record with no live locator but
//     LiveChunkCount > 0 — DefaultCommitBlock's last-wins DeleteSynced→MarkSynced
//     moved the hash onto a new block without decrementing this one, leaving its
//     count stuck > 0 forever, invisible to the hash-driven sweep (#1525).
//   - class 3 (ReclaimOrphanObjects): a remote blocks/<id> object with no backing
//     record, older than the grace window — PutBlock succeeded but the commit
//     never landed.
//
// A record with no live locator is terminally dead whatever its count: block IDs
// are fresh crypto/rand per carve and never reused, so no future locator can ever
// point back at it. Deleting it (and any lingering remote object) is therefore safe
// with no grace window. Class 3 alone needs a grace window, because a just-uploaded
// object may still have a commit in flight.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
)

// ReclaimMetaView is the per-share metadata surface the zero-ref reclaimer needs:
// the read-only reconcile view plus the record delete. metadata.Store satisfies
// it structurally.
type ReclaimMetaView interface {
	ReconcileMetaView
	DeleteBlockRecord(ctx context.Context, blockID string) error
}

// ReclaimOptions parameterizes a reclaim pass.
type ReclaimOptions struct {
	// DryRun classifies and tallies without deleting anything, so an operator
	// can preview the exact set a live run would act on.
	DryRun bool
	// SampleCap bounds the per-run ID sample. Zero defaults to
	// defaultReconcileSampleCap. Counts are always exact.
	SampleCap int
	// GracePeriod keeps freshly-uploaded, not-yet-committed remote objects from
	// being reclaimed as class-3 orphans (mirrors the GC sweep TTL). Non-positive
	// falls back to the sweep default via resolveGracePeriod. Ignored by
	// ReclaimRecords (records need no grace window).
	GracePeriod time.Duration
}

// ReclaimReport is the output of a reclaim pass.
type ReclaimReport struct {
	// Reclaimed tallies class-1 zero-ref records deleted (Count) and remote bytes
	// freed (Bytes, from each record's Length). On a dry run it tallies what WOULD
	// be deleted.
	Reclaimed ReconcileClass `json:"reclaimed"`
	// LeakedReclaimed tallies class-2 leaked block records deleted (#1525): no
	// live locator but a stale LiveChunkCount > 0.
	LeakedReclaimed ReconcileClass `json:"leaked_reclaimed"`
	// OrphanObjectsReclaimed tallies class-3 record-less remote objects deleted.
	OrphanObjectsReclaimed ReconcileClass `json:"orphan_objects_reclaimed"`
	// BlockRecordsScanned is every record examined across all shares.
	BlockRecordsScanned int64 `json:"block_records_scanned"`
	// RemoteObjectsScanned is every remote object examined for class 3.
	RemoteObjectsScanned int64 `json:"remote_objects_scanned"`
	// Errors counts orphans that could not be fully reclaimed (a DeleteBlock or
	// DeleteBlockRecord failure); each is left intact for the next run.
	Errors    int64 `json:"errors"`
	DryRun    bool  `json:"dry_run"`
	SampleCap int   `json:"sample_cap"`
}

// Merge folds other into r, aggregating per-remote passes into one server-wide
// report while respecting the sample cap.
func (r *ReclaimReport) Merge(other ReclaimReport) {
	cap := r.SampleCap
	if cap <= 0 {
		cap = defaultReconcileSampleCap
		r.SampleCap = cap
	}
	r.Reclaimed.merge(other.Reclaimed, cap)
	r.LeakedReclaimed.merge(other.LeakedReclaimed, cap)
	r.OrphanObjectsReclaimed.merge(other.OrphanObjectsReclaimed, cap)
	r.BlockRecordsScanned += other.BlockRecordsScanned
	r.RemoteObjectsScanned += other.RemoteObjectsScanned
	r.Errors += other.Errors
	if other.DryRun {
		r.DryRun = true
	}
}

// ReclaimRecords deletes class-1 and class-2 orphan records across the given
// per-share views that share one remote store: block records with no live locator,
// split by their (possibly stale) LiveChunkCount into zero-ref (== 0, class 1) and
// leaked (> 0, class 2 / #1525). For each it frees the remote block object
// (idempotent — the object may already be gone if a crash landed after DeleteBlock)
// then deletes the record, matching the GC reclaimer's DeleteBlock→DeleteBlockRecord
// order so a crash between them leaves a record-less orphan object (class 3, swept
// by ReclaimOrphanObjects) rather than a record pointing at freed bytes.
//
// It RE-DERIVES the classification here — never trusting a stale report — so a
// record re-referenced since the report is not deleted. Both classes are terminal:
// with no live locator, no future locator can point back (block IDs are never
// reused), so a stale count > 0 is safe to drop. rbs is the shared block store
// (nil = record-only reclaim; nothing to free remotely). The caller MUST hold the
// per-remote GC lock so this cannot race a concurrent sweep's DecrLiveChunkCount on
// the same block.
//
// A per-record delete failure is non-fatal: it is logged, counted in Errors, and
// the record is left intact for the next run. Only an enumerate/walk failure aborts
// the pass.
func ReclaimRecords(
	ctx context.Context,
	views []ReclaimMetaView,
	rbs remote.RemoteBlockStore,
	opts ReclaimOptions,
) (ReclaimReport, error) {
	sampleCap := opts.SampleCap
	if sampleCap <= 0 {
		sampleCap = defaultReconcileSampleCap
	}
	report := ReclaimReport{DryRun: opts.DryRun, SampleCap: sampleCap}

	for _, v := range views {
		// Collect EVERY record first, then build the live set, then filter. Order
		// matters for safety: a client commit (DefaultCommitBlock) writes a new
		// record AND its locators in one transaction, so if it lands while we are
		// scanning, walking records BEFORE building the live set guarantees any
		// record we captured has its locators visible to the later EnumerateSynced
		// — the live set can only be a superset of what was live at record-walk
		// time. Building the live set first would let a fresh commit slip in
		// between the two scans: its record (LiveChunkCount > 0) would be walked
		// but its BlockID missing from the stale live set, and we would delete a
		// live block. (Class 1 is incidentally immune — a fresh commit is never
		// count 0 — but class 2 has no such floor.)
		//
		// candidates maps blockID → its record meta; the live-set pass then
		// deletes referenced IDs from it, leaving only orphans. Records are
		// collected up front rather than acted on mid-walk because mutating the
		// record store during its own walk is unsafe on some backends.
		type candidate struct {
			length int64
			leaked bool // class 2 (stale count > 0) → tally into LeakedReclaimed
		}
		candidates := make(map[string]candidate)
		if err := v.WalkBlockRecords(ctx, func(rec block.BlockRecord) error {
			report.BlockRecordsScanned++
			candidates[rec.BlockID] = candidate{rec.Length, rec.LiveChunkCount != 0}
			return nil
		}); err != nil {
			return report, fmt.Errorf("reclaim: walk block records: %w", err)
		}

		// Any block ID a synced locator still points at is NOT an orphan, whatever
		// its record's counter says — drop it from the candidate set.
		//
		// Collect the hashes first and resolve their locators AFTER the enumerate
		// cursor closes: the sqlite metadata pool is MaxOpenConns(1), so calling
		// GetLocator inside the EnumerateSynced callback (while its rows cursor
		// still holds the only connection) would deadlock waiting for a second.
		var synced []block.ContentHash
		if err := v.EnumerateSynced(ctx, func(h block.ContentHash, _ time.Time) error {
			synced = append(synced, h)
			return nil
		}); err != nil {
			return report, fmt.Errorf("reclaim: enumerate synced: %w", err)
		}
		for _, h := range synced {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			loc, ok, err := v.GetLocator(ctx, h)
			if err != nil {
				return report, fmt.Errorf("reclaim: get locator: %w", err)
			}
			if ok && loc.BlockID != "" {
				delete(candidates, loc.BlockID)
			}
		}

		for blockID, c := range candidates {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			tally := &report.Reclaimed
			if c.leaked {
				tally = &report.LeakedReclaimed
			}
			if opts.DryRun {
				tally.add(blockID, c.length, sampleCap)
				continue
			}
			// DeleteBlock before DeleteBlockRecord: a crash between them leaves a
			// record-less orphan object (class 3), never a record pointing at
			// freed bytes. DeleteBlock is idempotent, so a block already freed by
			// the crash-before-DeleteBlockRecord path is a clean no-op.
			if rbs != nil {
				if err := rbs.DeleteBlock(ctx, blockID); err != nil {
					slog.Warn("reclaim: delete remote block failed — record kept for retry",
						"block_id", blockID, "err", err)
					report.Errors++
					continue
				}
			}
			if err := v.DeleteBlockRecord(ctx, blockID); err != nil {
				// The object may already be freed above; the record survives and
				// is retried next run. A now-record-less object is a class-3
				// orphan the object sweep reclaims.
				slog.Warn("reclaim: delete block record failed — will retry next run",
					"block_id", blockID, "err", err)
				report.Errors++
				continue
			}
			tally.add(blockID, c.length, sampleCap)
		}
	}

	if report.Reclaimed.Truncated || report.LeakedReclaimed.Truncated {
		slog.Warn("reclaim: ID sample truncated — full reclaimed set is larger than the sample cap",
			"sample_cap", sampleCap,
			"reclaimed", report.Reclaimed.Count,
			"leaked_reclaimed", report.LeakedReclaimed.Count)
	}
	return report, nil
}

// ReclaimOrphanObjects deletes class-3 orphans: remote blocks/<id> objects with no
// backing block record, older than the grace window. metaBlockIDs is the set of
// every block ID that HAS a record, unioned across ALL shares on this remote — the
// caller MUST build it from every share (not just the reclaim-eligible ones) and
// MUST NOT call this if any share on the remote could not be enumerated, or a
// sibling share's live object would be misread as an orphan and deleted.
//
// The grace window (mirroring the reporter and the GC walk sweep) protects a
// freshly-uploaded object whose commit may still be in flight; an object whose age
// cannot be evaluated (zero LastModified) is preserved fail-closed. The caller MUST
// hold the per-remote GC lock. A per-object DeleteBlock failure is non-fatal.
func ReclaimOrphanObjects(
	ctx context.Context,
	metaBlockIDs map[string]struct{},
	rbs remote.RemoteBlockStore,
	opts ReclaimOptions,
) (ReclaimReport, error) {
	sampleCap := opts.SampleCap
	if sampleCap <= 0 {
		sampleCap = defaultReconcileSampleCap
	}
	report := ReclaimReport{DryRun: opts.DryRun, SampleCap: sampleCap}
	if rbs == nil {
		return report, nil // remote cannot hold packed blocks — nothing to sweep
	}
	grace := resolveGracePeriod(&Options{GracePeriod: opts.GracePeriod})
	cutoff := time.Now().Add(-grace)

	// Collect during the walk, delete after: mutating the bucket mid-walk is
	// unsafe on some backends.
	type candidate struct {
		blockID string
		size    int64
	}
	var candidates []candidate
	if err := rbs.WalkBlocks(ctx, func(blockID string, meta block.Meta) error {
		report.RemoteObjectsScanned++
		if _, known := metaBlockIDs[blockID]; known {
			return nil // has a record — not an orphan
		}
		if meta.LastModified.IsZero() {
			return nil // cannot evaluate grace TTL — fail closed
		}
		if meta.LastModified.After(cutoff) {
			return nil // within grace: commit may still land
		}
		candidates = append(candidates, candidate{blockID, meta.Size})
		return nil
	}); err != nil {
		return report, fmt.Errorf("reclaim: walk blocks: %w", err)
	}

	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if opts.DryRun {
			report.OrphanObjectsReclaimed.add(c.blockID, c.size, sampleCap)
			continue
		}
		if err := rbs.DeleteBlock(ctx, c.blockID); err != nil {
			slog.Warn("reclaim: delete orphan object failed — will retry next run",
				"block_id", c.blockID, "err", err)
			report.Errors++
			continue
		}
		report.OrphanObjectsReclaimed.add(c.blockID, c.size, sampleCap)
	}

	if report.OrphanObjectsReclaimed.Truncated {
		slog.Warn("reclaim: orphan-object ID sample truncated — full set is larger than the sample cap",
			"sample_cap", sampleCap, "orphan_objects_reclaimed", report.OrphanObjectsReclaimed.Count)
	}
	return report, nil
}
