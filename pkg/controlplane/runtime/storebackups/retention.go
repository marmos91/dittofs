package storebackups

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/backup"
	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// DefaultJobRetention is the age threshold for BackupJob pruning (D-17).
// BackupJob rows with FinishedAt older than this cutoff are deleted on
// every retention pass.
const DefaultJobRetention = 30 * 24 * time.Hour

// DefaultMinKeepSucceeded is the safety floor enforced by D-11 / SCHED-05.
// Retention will never allow the post-prune count of succeeded records
// (pinned + non-pinned) to drop below this value. Not operator-configurable
// in v0.13.0 — losing all restorable archives is strictly worse than
// retaining one stale backup.
const DefaultMinKeepSucceeded = 1

// RetentionStore is the narrow slice of store.BackupStore the retention
// pass touches. Declared here so tests can provide an in-memory fake
// without implementing the full composite store interface.
type RetentionStore interface {
	ListSucceededRecordsForRetention(ctx context.Context, repoID string) ([]*models.BackupRecord, error)
	ListBackupRecordsByRepo(ctx context.Context, repoID string) ([]*models.BackupRecord, error)
	DeleteBackupRecord(ctx context.Context, id string) error
	PruneBackupJobsOlderThan(ctx context.Context, cutoff time.Time) (int, error)
}

// Clock is re-exported from pkg/backup so retention_test.go and existing
// callers don't have to rename their references. The real clock is
// backup.RealClock{}; tests inject their own.
type Clock = backup.Clock

// RetentionReport summarizes a retention pass outcome for the caller
// (Plan 05's RunBackup path). Per D-15 these failures do NOT degrade
// the parent BackupJob status — they surface via logs and this report.
type RetentionReport struct {
	RepoID        string
	Considered    int              // total non-pinned succeeded records evaluated
	Deleted       []string         // record IDs successfully pruned
	SkippedPinned int              // count of pinned succeeded records (outside count math)
	SkippedSafety int              // count of records kept by the D-11 safety rail
	FailedDeletes map[string]error // per-record delete errors (D-13 continue-on-error)
	JobsPruned    int              // count of BackupJob rows pruned (D-17)
}

// retentionDecision records per-candidate retention state. The three flags
// are OR-combined: a record is KEPT iff any is true; otherwise it is pruned.
type retentionDecision struct {
	rec          *models.BackupRecord
	keptByCount  bool
	keptByAge    bool
	keptBySafety bool
}

// RunRetention prunes a repo's backup history per D-08..D-14 and prunes
// old BackupJob rows per D-17. Called inline from Plan 05 after a successful
// backup under the per-repo mutex (D-08, SCHED-06).
//
// Policy (D-09 UNION): a record is RETAINED if it matches EITHER
//
//	(a) top-N by created_at (newest) where N = repo.KeepCount, OR
//	(b) created_at >= now - repo.KeepAgeDays.
//
// Pinned records are OUTSIDE the count math (D-10) — they are never pruned
// and never consume a keep-count slot.
//
// Safety rail (D-11): if pruning would leave ZERO succeeded records for
// the repo (pinned + non-pinned), the newest candidate is retained instead.
// The one-succeeded floor is inviolable over age policy.
//
// Destination-first (D-14): Destination.Delete(id) runs first; only on
// success does the DB row get removed. A destination failure leaves the
// DB row in place for the NEXT retention pass to retry (D-13).
//
// Returns a RetentionReport regardless of outcome; never aborts on a
// per-record error. If the initial record-enumeration query fails (rare),
// the returned error is non-nil and the report is partial.
func RunRetention(
	ctx context.Context,
	repo *models.BackupRepo,
	dst destination.Destination,
	store RetentionStore,
	clock Clock,
) (RetentionReport, error) {
	if clock == nil {
		clock = backup.RealClock{}
	}
	report := RetentionReport{
		RepoID:        repo.ID,
		FailedDeletes: map[string]error{},
	}

	now := clock.Now()

	// Step 0: no-policy fast path. Still run the job pruner (D-17).
	if (repo.KeepCount == nil || *repo.KeepCount <= 0) && (repo.KeepAgeDays == nil || *repo.KeepAgeDays <= 0) {
		pruned, pruneErr := store.PruneBackupJobsOlderThan(ctx, now.Add(-DefaultJobRetention))
		report.JobsPruned = pruned
		if pruneErr != nil {
			logger.Warn("Failed to prune old backup jobs", "repo_id", repo.ID, "error", pruneErr)
		}
		return report, nil
	}

	// Step 1: candidate set = succeeded AND NOT pinned, oldest-first (D-10, D-12).
	candidates, err := store.ListSucceededRecordsForRetention(ctx, repo.ID)
	if err != nil {
		return report, fmt.Errorf("list retention candidates: %w", err)
	}
	report.Considered = len(candidates)

	// Step 2: count ALL succeeded records (pinned + non-pinned) for the safety
	// rail (D-11). Pinned records provide a succeeded-archive floor — if any
	// exist, the safety rail does not need to save a non-pinned candidate.
	allRecords, err := store.ListBackupRecordsByRepo(ctx, repo.ID)
	if err != nil {
		return report, fmt.Errorf("list all records: %w", err)
	}
	var totalSucceeded int
	for _, r := range allRecords {
		if r.Status == models.BackupStatusSucceeded {
			totalSucceeded++
		}
		if r.Pinned && r.Status == models.BackupStatusSucceeded {
			report.SkippedPinned++
		}
	}

	// Step 3: compute per-candidate keep decisions.
	// D-09 UNION: keep if (index_from_newest < KeepCount) OR (CreatedAt >= now - KeepAgeDays).
	// Candidates are sorted oldest-first (ASC); "top-N newest" means index >= (len - N).
	keepCount := 0
	if repo.KeepCount != nil && *repo.KeepCount > 0 {
		keepCount = *repo.KeepCount
	}
	var ageCutoff time.Time
	ageEnabled := repo.KeepAgeDays != nil && *repo.KeepAgeDays > 0
	if ageEnabled {
		ageCutoff = now.AddDate(0, 0, -*repo.KeepAgeDays)
	}

	decisions := make([]retentionDecision, len(candidates))
	for i, rec := range candidates {
		d := retentionDecision{rec: rec}
		if keepCount > 0 && i >= len(candidates)-keepCount {
			d.keptByCount = true
		}
		if ageEnabled && !rec.CreatedAt.Before(ageCutoff) {
			d.keptByAge = true
		}
		decisions[i] = d
	}

	// Step 4: safety rail (D-11, SCHED-05). Compute post-prune succeeded count
	// and, if it would fall below DefaultMinKeepSucceeded, rescue the newest
	// deletable candidate. Iterate from the end (newest-first) so the rescued
	// record is the one with the freshest data.
	willDelete := 0
	for _, d := range decisions {
		if !d.keptByCount && !d.keptByAge {
			willDelete++
		}
	}
	postPruneSucceeded := totalSucceeded - willDelete
	if postPruneSucceeded < DefaultMinKeepSucceeded {
		for i := len(decisions) - 1; i >= 0; i-- {
			if !decisions[i].keptByCount && !decisions[i].keptByAge {
				decisions[i].keptBySafety = true
				report.SkippedSafety++
				logger.Warn("Retention kept candidate via safety rail",
					"repo_id", repo.ID,
					"record_id", decisions[i].rec.ID,
					"created_at", decisions[i].rec.CreatedAt)
				break
			}
		}
	}

	// Step 5: perform deletions, destination-first (D-14), continue-on-error (D-13).
	for _, d := range decisions {
		if d.keptByCount || d.keptByAge || d.keptBySafety {
			continue
		}
		if ctx.Err() != nil {
			// Context cancelled mid-retention — bail out but preserve already-done
			// work. D-15: retention failures don't degrade parent job; return report.
			logger.Warn("Retention pass cancelled", "repo_id", repo.ID, "error", ctx.Err())
			break
		}

		// Destination first (D-14).
		if err := dst.Delete(ctx, d.rec.ID); err != nil {
			report.FailedDeletes[d.rec.ID] = err
			logger.Warn("Destination delete failed; DB row retained for retry",
				"repo_id", repo.ID, "record_id", d.rec.ID, "error", err)
			continue
		}

		// DB row next. A failure here means the destination archive is gone but
		// the DB row remains — the next pass will retry Delete (idempotent on
		// missing manifests) then retry the DB delete. No orphaned-archive leak.
		if err := store.DeleteBackupRecord(ctx, d.rec.ID); err != nil {
			report.FailedDeletes[d.rec.ID] = fmt.Errorf("destination deleted, DB retain: %w", err)
			logger.Warn("Destination deleted but DB delete failed",
				"repo_id", repo.ID, "record_id", d.rec.ID, "error", err)
			continue
		}

		report.Deleted = append(report.Deleted, d.rec.ID)
		logger.Info("Retention deleted record",
			"repo_id", repo.ID, "record_id", d.rec.ID, "created_at", d.rec.CreatedAt)
	}

	// Step 6: D-17 BackupJob pruner — 30-day rolling window. Runs every
	// retention pass; cheap, bounded. Errors logged-and-continued so
	// retention.failure never degrades the parent job.
	pruned, pruneErr := store.PruneBackupJobsOlderThan(ctx, now.Add(-DefaultJobRetention))
	report.JobsPruned = pruned
	if pruneErr != nil {
		logger.Warn("BackupJob pruner failed", "repo_id", repo.ID, "error", pruneErr)
	}

	return report, nil
}
