// Package engine — zero-ref record reclaim (#1493/#1525 reconcile, PR5b), the
// first *deleting* stage after the read-only reporter (reconcile.go). It reclaims
// class-1 orphans: block records with LiveChunkCount == 0 and no live locator — a
// crash between DecrLiveChunkCount and DeleteBlockRecord. Such a record is
// terminally dead (the count only ever decrements and block IDs are never reused),
// so deleting it and any lingering remote object is safe with no grace window.
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

// ReclaimOptions parameterizes a zero-ref reclaim pass.
type ReclaimOptions struct {
	// DryRun classifies and tallies without deleting anything, so an operator
	// can preview the exact set a live run would act on.
	DryRun bool
	// SampleCap bounds the per-run ID sample. Zero defaults to
	// defaultReconcileSampleCap. Counts are always exact.
	SampleCap int
}

// ReclaimReport is the output of a zero-ref reclaim pass.
type ReclaimReport struct {
	// Reclaimed tallies the zero-ref records deleted (Count) and the remote
	// bytes freed (Bytes, from each record's Length). On a dry run it tallies
	// what WOULD be deleted.
	Reclaimed ReconcileClass `json:"reclaimed"`
	// BlockRecordsScanned is every record examined across all shares.
	BlockRecordsScanned int64 `json:"block_records_scanned"`
	// Errors counts records that could not be fully reclaimed (a DeleteBlock or
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
	r.BlockRecordsScanned += other.BlockRecordsScanned
	r.Errors += other.Errors
	if other.DryRun {
		r.DryRun = true
	}
}

// ReclaimZeroRefRecords deletes class-1 orphans across the given per-share views
// that share one remote store: block records with LiveChunkCount == 0 and no live
// locator. For each it frees the remote block object (idempotent — the object may
// already be gone if the crash landed after DeleteBlock) then deletes the record,
// matching the GC reclaimer's DeleteBlock→DeleteBlockRecord order so a crash
// between them leaves a record-less orphan object (class 3, swept later) rather
// than a record pointing at freed bytes.
//
// It RE-DERIVES the classification here — never trusting a stale report — so a
// record re-referenced since the report is not deleted. rbs is the shared block
// store (nil = record-only reclaim; nothing to free remotely). The caller MUST
// hold the per-remote GC lock so this cannot race a concurrent sweep's
// DecrLiveChunkCount on the same block.
//
// A per-record delete failure is non-fatal: it is logged, counted in Errors, and
// the record is left intact for the next run. Only an enumerate/walk failure
// aborts the pass.
func ReclaimZeroRefRecords(
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
		// Live locator set for THIS share: any block ID a synced locator still
		// points at is NOT a zero-ref orphan, whatever its counter says.
		refSet := make(map[string]struct{})
		if err := v.EnumerateSynced(ctx, func(h block.ContentHash, _ time.Time) error {
			loc, ok, err := v.GetLocator(ctx, h)
			if err != nil {
				return err
			}
			if ok && loc.BlockID != "" {
				refSet[loc.BlockID] = struct{}{}
			}
			return nil
		}); err != nil {
			return report, fmt.Errorf("reclaim: enumerate synced: %w", err)
		}

		// Collect candidates during the walk and delete AFTER it: mutating the
		// record store mid-walk is unsafe on some backends.
		type candidate struct {
			blockID string
			length  int64
		}
		var candidates []candidate
		if err := v.WalkBlockRecords(ctx, func(rec block.BlockRecord) error {
			report.BlockRecordsScanned++
			if _, live := refSet[rec.BlockID]; live {
				return nil // healthy: a live locator still points at this block
			}
			if rec.LiveChunkCount == 0 {
				candidates = append(candidates, candidate{rec.BlockID, rec.Length})
			}
			return nil
		}); err != nil {
			return report, fmt.Errorf("reclaim: walk block records: %w", err)
		}

		for _, c := range candidates {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			if opts.DryRun {
				report.Reclaimed.add(c.blockID, c.length, sampleCap)
				continue
			}
			// DeleteBlock before DeleteBlockRecord: a crash between them leaves a
			// record-less orphan object (class 3), never a record pointing at
			// freed bytes. DeleteBlock is idempotent, so a block already freed by
			// the crash-before-DeleteBlockRecord path is a clean no-op.
			if rbs != nil {
				if err := rbs.DeleteBlock(ctx, c.blockID); err != nil {
					slog.Warn("reclaim: delete remote block failed — record kept for retry",
						"block_id", c.blockID, "err", err)
					report.Errors++
					continue
				}
			}
			if err := v.DeleteBlockRecord(ctx, c.blockID); err != nil {
				// The object may already be freed above; the record survives and
				// is retried next run. A now-record-less object is a class-3
				// orphan the object sweep reclaims.
				slog.Warn("reclaim: delete block record failed — will retry next run",
					"block_id", c.blockID, "err", err)
				report.Errors++
				continue
			}
			report.Reclaimed.add(c.blockID, c.length, sampleCap)
		}
	}

	if report.Reclaimed.Truncated {
		slog.Warn("reclaim: ID sample truncated — full reclaimed set is larger than the sample cap",
			"sample_cap", sampleCap, "reclaimed", report.Reclaimed.Count)
	}
	return report, nil
}
