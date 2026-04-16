package storebackups

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/manifest"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ---- Fakes ----

// fixedClock is a deterministic clock for time-sensitive retention tests.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// fakeStore is an in-memory RetentionStore for testing.
type fakeStore struct {
	mu             sync.Mutex
	records        map[string]*models.BackupRecord
	jobs           map[string]*models.BackupJob
	deleteErrs     map[string]error // record_id → error to inject on DeleteBackupRecord
	onRecordDelete func(id string)  // call-order hook for T9
	listErr        error            // inject error on ListSucceededRecordsForRetention
	listAllErr     error            // inject error on ListBackupRecordsByRepo
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		records:    make(map[string]*models.BackupRecord),
		jobs:       make(map[string]*models.BackupJob),
		deleteErrs: make(map[string]error),
	}
}

func (f *fakeStore) ListSucceededRecordsForRetention(ctx context.Context, repoID string) ([]*models.BackupRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []*models.BackupRecord
	for _, r := range f.records {
		if r.RepoID == repoID && r.Status == models.BackupStatusSucceeded && !r.Pinned {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (f *fakeStore) ListBackupRecordsByRepo(ctx context.Context, repoID string) ([]*models.BackupRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listAllErr != nil {
		return nil, f.listAllErr
	}
	var out []*models.BackupRecord
	for _, r := range f.records {
		if r.RepoID == repoID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (f *fakeStore) DeleteBackupRecord(ctx context.Context, id string) error {
	f.mu.Lock()
	hook := f.onRecordDelete
	injected, hasErr := f.deleteErrs[id]
	f.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if hasErr && injected != nil {
		return injected
	}
	if _, ok := f.records[id]; !ok {
		return models.ErrBackupRecordNotFound
	}
	delete(f.records, id)
	return nil
}

func (f *fakeStore) PruneBackupJobsOlderThan(ctx context.Context, cutoff time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	deleted := 0
	for id, j := range f.jobs {
		if j.FinishedAt != nil && j.FinishedAt.Before(cutoff) {
			delete(f.jobs, id)
			deleted++
		}
	}
	return deleted, nil
}

// fakeDst is an in-memory destination.Destination for retention tests.
// Only Delete is exercised; the remaining methods are stubs.
type fakeDst struct {
	mu          sync.Mutex
	deleteCalls []string
	deleteErrs  map[string]error
	onDelete    func(id string)
}

func newFakeDst() *fakeDst {
	return &fakeDst{deleteErrs: make(map[string]error)}
}

func (d *fakeDst) PutBackup(ctx context.Context, m *manifest.Manifest, payload io.Reader) error {
	return nil
}

func (d *fakeDst) GetBackup(ctx context.Context, id string) (*manifest.Manifest, io.ReadCloser, error) {
	return nil, nil, errors.New("not implemented in fake")
}

func (d *fakeDst) GetManifestOnly(ctx context.Context, id string) (*manifest.Manifest, error) {
	return nil, errors.New("not implemented in fake")
}

func (d *fakeDst) List(ctx context.Context) ([]destination.BackupDescriptor, error) {
	return nil, nil
}

func (d *fakeDst) Stat(ctx context.Context, id string) (*destination.BackupDescriptor, error) {
	return nil, nil
}

func (d *fakeDst) Delete(ctx context.Context, id string) error {
	d.mu.Lock()
	hook := d.onDelete
	injected, hasErr := d.deleteErrs[id]
	d.deleteCalls = append(d.deleteCalls, id)
	d.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	if hasErr {
		return injected
	}
	return nil
}

func (d *fakeDst) ValidateConfig(ctx context.Context) error { return nil }
func (d *fakeDst) Close() error                             { return nil }

// Compile-time check: *fakeDst implements destination.Destination.
var _ destination.Destination = (*fakeDst)(nil)

// ---- Helpers ----

// seedSuccessRecords creates n succeeded non-pinned records, oldest-first,
// spaced by delta starting at baseTime. Returns records in oldest-first order.
func seedSuccessRecords(store *fakeStore, repoID string, n int, baseTime time.Time, delta time.Duration) []*models.BackupRecord {
	store.mu.Lock()
	defer store.mu.Unlock()
	out := make([]*models.BackupRecord, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("rec-%s-%02d", repoID, i)
		r := &models.BackupRecord{
			ID:        id,
			RepoID:    repoID,
			CreatedAt: baseTime.Add(time.Duration(i) * delta),
			Status:    models.BackupStatusSucceeded,
			Pinned:    false,
			SizeBytes: 1024,
		}
		store.records[id] = r
		out = append(out, r)
	}
	return out
}

func addRecord(store *fakeStore, id, repoID string, status models.BackupStatus, pinned bool, createdAt time.Time) *models.BackupRecord {
	store.mu.Lock()
	defer store.mu.Unlock()
	r := &models.BackupRecord{
		ID:        id,
		RepoID:    repoID,
		CreatedAt: createdAt,
		Status:    status,
		Pinned:    pinned,
	}
	store.records[id] = r
	return r
}

func addJob(store *fakeStore, id, repoID string, status models.BackupStatus, finishedAt *time.Time) *models.BackupJob {
	store.mu.Lock()
	defer store.mu.Unlock()
	j := &models.BackupJob{
		ID:         id,
		Kind:       models.BackupJobKindBackup,
		RepoID:     repoID,
		Status:     status,
		FinishedAt: finishedAt,
	}
	store.jobs[id] = j
	return j
}

// ---- TESTS ----

func TestRunRetention(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)
	clock := fixedClock{t: now}
	repoID := "repo-alpha"

	t.Run("T1_no_policy_does_nothing", func(t *testing.T) {
		store := newFakeStore()
		dst := newFakeDst()
		seedSuccessRecords(store, repoID, 10, now.Add(-48*time.Hour), time.Hour)

		repo := &models.BackupRepo{ID: repoID, KeepCount: nil, KeepAgeDays: nil}

		report, err := RunRetention(ctx, repo, dst, store, clock)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(report.Deleted) != 0 {
			t.Fatalf("expected 0 deletions, got %d: %v", len(report.Deleted), report.Deleted)
		}
		if len(report.FailedDeletes) != 0 {
			t.Fatalf("expected 0 failures, got %v", report.FailedDeletes)
		}
	})

	t.Run("T2_count_only_deletes_oldest", func(t *testing.T) {
		store := newFakeStore()
		dst := newFakeDst()
		recs := seedSuccessRecords(store, repoID, 10, now.Add(-48*time.Hour), time.Hour)

		keep := 5
		repo := &models.BackupRepo{ID: repoID, KeepCount: &keep, KeepAgeDays: nil}

		report, err := RunRetention(ctx, repo, dst, store, clock)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(report.Deleted) != 5 {
			t.Fatalf("expected 5 deletions, got %d: %v", len(report.Deleted), report.Deleted)
		}
		expectedDeleted := map[string]struct{}{}
		for i := 0; i < 5; i++ {
			expectedDeleted[recs[i].ID] = struct{}{}
		}
		for _, id := range report.Deleted {
			if _, ok := expectedDeleted[id]; !ok {
				t.Errorf("unexpected deleted id: %s", id)
			}
		}
		store.mu.Lock()
		remaining := len(store.records)
		store.mu.Unlock()
		if remaining != 5 {
			t.Errorf("expected 5 remaining, got %d", remaining)
		}
	})

	t.Run("T3_age_only_deletes_old", func(t *testing.T) {
		store := newFakeStore()
		dst := newFakeDst()
		baseTime := now.Add(-20 * 24 * time.Hour)
		seedSuccessRecords(store, repoID, 10, baseTime, 2*24*time.Hour)

		keepAge := 7
		repo := &models.BackupRepo{ID: repoID, KeepCount: nil, KeepAgeDays: &keepAge}

		report, err := RunRetention(ctx, repo, dst, store, clock)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Age cutoff = now - 7d. Records with CreatedAt < cutoff are deleted.
		// CreatedAt = baseTime + i*2d = (now - 20d) + i*2d. < (now - 7d) iff i*2 < 13 iff i ≤ 6
		// → indices 0..6 deleted (7 records).
		if len(report.Deleted) != 7 {
			t.Fatalf("expected 7 deletions, got %d: %v", len(report.Deleted), report.Deleted)
		}
	})

	t.Run("T4_union_age_keeps_all", func(t *testing.T) {
		store := newFakeStore()
		dst := newFakeDst()
		baseTime := now.Add(-5 * 24 * time.Hour)
		seedSuccessRecords(store, repoID, 10, baseTime, 6*time.Hour)

		keep := 3
		keepAge := 30
		repo := &models.BackupRepo{ID: repoID, KeepCount: &keep, KeepAgeDays: &keepAge}

		report, err := RunRetention(ctx, repo, dst, store, clock)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(report.Deleted) != 0 {
			t.Fatalf("expected 0 deletions (union keeps all within 30d), got %d: %v", len(report.Deleted), report.Deleted)
		}
	})

	t.Run("T5_union_both_active", func(t *testing.T) {
		store := newFakeStore()
		dst := newFakeDst()
		// 10 records spanning 60 days: oldest = now - 60d, newest = now - 6d.
		baseTime := now.Add(-60 * 24 * time.Hour)
		recs := seedSuccessRecords(store, repoID, 10, baseTime, 6*24*time.Hour)

		keep := 3
		keepAge := 7
		repo := &models.BackupRepo{ID: repoID, KeepCount: &keep, KeepAgeDays: &keepAge}

		report, err := RunRetention(ctx, repo, dst, store, clock)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Union: keptByCount = i >= 10-3 = 7 → indexes 7,8,9.
		//        keptByAge   = CreatedAt >= now-7d → baseTime + i*6d >= now-7d
		//                      (now-60d) + i*6d >= now-7d iff i*6 >= 53 iff i >= 9.
		// Union keeps {7, 8, 9} → 3 kept, 7 deleted.
		if len(report.Deleted) != 7 {
			t.Fatalf("expected 7 deletions, got %d: %v", len(report.Deleted), report.Deleted)
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		for i := 7; i < 10; i++ {
			if _, ok := store.records[recs[i].ID]; !ok {
				t.Errorf("expected record %s (index %d) to remain", recs[i].ID, i)
			}
		}
	})

	t.Run("T6_pinned_skip", func(t *testing.T) {
		store := newFakeStore()
		dst := newFakeDst()
		// 7 non-pinned succeeded + 3 pinned succeeded = 10 total.
		baseTime := now.Add(-48 * time.Hour)
		seedSuccessRecords(store, repoID, 7, baseTime, time.Hour)
		addRecord(store, "pin-1", repoID, models.BackupStatusSucceeded, true, baseTime.Add(-10*time.Hour))
		addRecord(store, "pin-2", repoID, models.BackupStatusSucceeded, true, baseTime.Add(-11*time.Hour))
		addRecord(store, "pin-3", repoID, models.BackupStatusSucceeded, true, baseTime.Add(-12*time.Hour))

		keep := 5
		repo := &models.BackupRepo{ID: repoID, KeepCount: &keep, KeepAgeDays: nil}

		report, err := RunRetention(ctx, repo, dst, store, clock)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Candidates = 7 non-pinned; keep=5 → 2 oldest non-pinned get deleted.
		if len(report.Deleted) != 2 {
			t.Fatalf("expected 2 deletions (7 non-pinned - 5 kept), got %d: %v", len(report.Deleted), report.Deleted)
		}
		// Pinned records remain.
		store.mu.Lock()
		defer store.mu.Unlock()
		for _, pid := range []string{"pin-1", "pin-2", "pin-3"} {
			if _, ok := store.records[pid]; !ok {
				t.Errorf("pinned record %s was deleted (must never happen)", pid)
			}
		}
		if report.SkippedPinned != 3 {
			t.Errorf("expected SkippedPinned=3, got %d", report.SkippedPinned)
		}
	})

	t.Run("T7_safety_rail_keeps_only_old_record", func(t *testing.T) {
		store := newFakeStore()
		dst := newFakeDst()
		addRecord(store, "only-one", repoID, models.BackupStatusSucceeded, false, now.Add(-100*24*time.Hour))

		keepAge := 7
		repo := &models.BackupRepo{ID: repoID, KeepCount: nil, KeepAgeDays: &keepAge}

		report, err := RunRetention(ctx, repo, dst, store, clock)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(report.Deleted) != 0 {
			t.Fatalf("safety rail violated: expected 0 deletions, got %d: %v", len(report.Deleted), report.Deleted)
		}
		if report.SkippedSafety != 1 {
			t.Errorf("expected SkippedSafety=1, got %d", report.SkippedSafety)
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		if _, ok := store.records["only-one"]; !ok {
			t.Errorf("safety rail: only-one record was deleted")
		}
	})

	t.Run("T8_succeeded_only_considered", func(t *testing.T) {
		store := newFakeStore()
		dst := newFakeDst()
		baseTime := now.Add(-48 * time.Hour)
		addRecord(store, "s1", repoID, models.BackupStatusSucceeded, false, baseTime)
		addRecord(store, "s2", repoID, models.BackupStatusSucceeded, false, baseTime.Add(1*time.Hour))
		addRecord(store, "s3", repoID, models.BackupStatusSucceeded, false, baseTime.Add(2*time.Hour))
		addRecord(store, "f1", repoID, models.BackupStatusFailed, false, baseTime.Add(3*time.Hour))
		addRecord(store, "f2", repoID, models.BackupStatusFailed, false, baseTime.Add(4*time.Hour))
		addRecord(store, "i1", repoID, models.BackupStatusInterrupted, false, baseTime.Add(5*time.Hour))

		keep := 1
		repo := &models.BackupRepo{ID: repoID, KeepCount: &keep, KeepAgeDays: nil}

		report, err := RunRetention(ctx, repo, dst, store, clock)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if report.Considered != 3 {
			t.Errorf("expected Considered=3 (only succeeded), got %d", report.Considered)
		}
		if len(report.Deleted) != 2 {
			t.Fatalf("expected 2 deletions (s1, s2), got %d: %v", len(report.Deleted), report.Deleted)
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		for _, fid := range []string{"f1", "f2", "i1"} {
			if _, ok := store.records[fid]; !ok {
				t.Errorf("non-succeeded record %s was deleted (must not happen)", fid)
			}
		}
	})

	t.Run("T9_destination_first_delete_order", func(t *testing.T) {
		store := newFakeStore()
		dst := newFakeDst()
		baseTime := now.Add(-48 * time.Hour)
		seedSuccessRecords(store, repoID, 5, baseTime, time.Hour)

		var callOrder []string
		var callOrderMu sync.Mutex
		record := func(tag, id string) {
			callOrderMu.Lock()
			defer callOrderMu.Unlock()
			callOrder = append(callOrder, tag+":"+id)
		}
		store.onRecordDelete = func(id string) { record("db", id) }
		dst.onDelete = func(id string) { record("dst", id) }

		keep := 2
		repo := &models.BackupRepo{ID: repoID, KeepCount: &keep, KeepAgeDays: nil}

		report, err := RunRetention(ctx, repo, dst, store, clock)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(report.Deleted) != 3 {
			t.Fatalf("expected 3 deletions, got %d", len(report.Deleted))
		}

		callOrderMu.Lock()
		defer callOrderMu.Unlock()
		// For each db: entry, a matching dst: entry must precede it.
		seenDst := map[string]int{}
		dstCount, dbCount := 0, 0
		for i, ev := range callOrder {
			switch {
			case len(ev) > 4 && ev[:4] == "dst:":
				seenDst[ev[4:]] = i
				dstCount++
			case len(ev) > 3 && ev[:3] == "db:":
				id := ev[3:]
				dstIdx, ok := seenDst[id]
				if !ok {
					t.Errorf("db:%s called without preceding dst:%s (call order: %v)", id, id, callOrder)
					continue
				}
				if dstIdx >= i {
					t.Errorf("dst:%s at index %d did not precede db:%s at index %d", id, dstIdx, id, i)
				}
				dbCount++
			}
		}
		if dstCount != 3 || dbCount != 3 {
			t.Errorf("expected 3 dst + 3 db calls, got dst=%d db=%d", dstCount, dbCount)
		}
	})

	t.Run("T10_destination_failure_preserves_db_row", func(t *testing.T) {
		store := newFakeStore()
		dst := newFakeDst()
		baseTime := now.Add(-48 * time.Hour)
		recs := seedSuccessRecords(store, repoID, 5, baseTime, time.Hour)

		failID := recs[0].ID
		dst.deleteErrs[failID] = fmt.Errorf("%w: simulated S3 outage", destination.ErrDestinationUnavailable)

		keep := 2
		repo := &models.BackupRepo{ID: repoID, KeepCount: &keep, KeepAgeDays: nil}

		report, err := RunRetention(ctx, repo, dst, store, clock)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(report.Deleted) != 2 {
			t.Errorf("expected 2 deletions, got %d: %v", len(report.Deleted), report.Deleted)
		}
		if _, ok := report.FailedDeletes[failID]; !ok {
			t.Errorf("expected FailedDeletes[%s] to be set", failID)
		}
		// DB row must remain.
		store.mu.Lock()
		defer store.mu.Unlock()
		if _, ok := store.records[failID]; !ok {
			t.Errorf("destination-first violated: DB row %s was deleted despite destination failure", failID)
		}
	})

	t.Run("T11_continue_on_error", func(t *testing.T) {
		store := newFakeStore()
		dst := newFakeDst()
		baseTime := now.Add(-48 * time.Hour)
		recs := seedSuccessRecords(store, repoID, 7, baseTime, time.Hour)

		// keep=2 → 5 deletions. Fail the 2nd deletion target (index 1).
		failID := recs[1].ID
		dst.deleteErrs[failID] = errors.New("ephemeral dest failure")

		keep := 2
		repo := &models.BackupRepo{ID: repoID, KeepCount: &keep, KeepAgeDays: nil}

		report, err := RunRetention(ctx, repo, dst, store, clock)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(report.Deleted) != 4 {
			t.Errorf("expected 4 successful deletes, got %d: %v", len(report.Deleted), report.Deleted)
		}
		if len(report.FailedDeletes) != 1 {
			t.Errorf("expected 1 failed delete, got %d: %v", len(report.FailedDeletes), report.FailedDeletes)
		}
		if _, ok := report.FailedDeletes[failID]; !ok {
			t.Errorf("expected FailedDeletes[%s] to be set", failID)
		}
	})

	t.Run("T12_pinned_provides_safety_floor", func(t *testing.T) {
		store := newFakeStore()
		dst := newFakeDst()
		oldTime := now.Add(-100 * 24 * time.Hour)
		addRecord(store, "np-1", repoID, models.BackupStatusSucceeded, false, oldTime)
		addRecord(store, "np-2", repoID, models.BackupStatusSucceeded, false, oldTime.Add(1*time.Hour))
		addRecord(store, "pin-1", repoID, models.BackupStatusSucceeded, true, oldTime.Add(2*time.Hour))

		keepAge := 7
		repo := &models.BackupRepo{ID: repoID, KeepCount: nil, KeepAgeDays: &keepAge}

		report, err := RunRetention(ctx, repo, dst, store, clock)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(report.Deleted) != 2 {
			t.Errorf("expected 2 deletions, got %d: %v", len(report.Deleted), report.Deleted)
		}
		if report.SkippedSafety != 0 {
			t.Errorf("expected SkippedSafety=0 (pinned is the floor), got %d", report.SkippedSafety)
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		if _, ok := store.records["pin-1"]; !ok {
			t.Errorf("pinned record was deleted (must never happen)")
		}
		for _, id := range []string{"np-1", "np-2"} {
			if _, ok := store.records[id]; ok {
				t.Errorf("non-pinned record %s should have been deleted", id)
			}
		}
	})
}

func TestPruneOldJobs(t *testing.T) {
	ctx := context.Background()
	repoID := "repo-alpha"

	t.Run("T13_prunes_old_finished_jobs", func(t *testing.T) {
		store := newFakeStore()
		now := time.Now().UTC()
		oldTime := now.Add(-40 * 24 * time.Hour)
		recentTime := now.Add(-10 * 24 * time.Hour)
		for i := 0; i < 5; i++ {
			t2 := oldTime.Add(time.Duration(i) * time.Hour)
			addJob(store, fmt.Sprintf("old-%d", i), repoID, models.BackupStatusSucceeded, &t2)
		}
		for i := 0; i < 3; i++ {
			t2 := recentTime.Add(time.Duration(i) * time.Hour)
			addJob(store, fmt.Sprintf("new-%d", i), repoID, models.BackupStatusSucceeded, &t2)
		}

		count, err := store.PruneBackupJobsOlderThan(ctx, time.Now().UTC().Add(-30*24*time.Hour))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if count != 5 {
			t.Errorf("expected count=5 pruned, got %d", count)
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		if len(store.jobs) != 3 {
			t.Errorf("expected 3 jobs remaining, got %d", len(store.jobs))
		}
		for _, id := range []string{"new-0", "new-1", "new-2"} {
			if _, ok := store.jobs[id]; !ok {
				t.Errorf("recent job %s was unexpectedly pruned", id)
			}
		}
	})

	t.Run("T14_preserves_running_and_pending", func(t *testing.T) {
		store := newFakeStore()
		addJob(store, "running-1", repoID, models.BackupStatusRunning, nil)
		addJob(store, "pending-1", repoID, models.BackupStatusPending, nil)
		oldFinish := time.Now().UTC().Add(-60 * 24 * time.Hour)
		addJob(store, "old-done", repoID, models.BackupStatusSucceeded, &oldFinish)

		_, err := store.PruneBackupJobsOlderThan(ctx, time.Now().UTC().Add(-30*24*time.Hour))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		if _, ok := store.jobs["running-1"]; !ok {
			t.Errorf("running job was pruned (must never happen)")
		}
		if _, ok := store.jobs["pending-1"]; !ok {
			t.Errorf("pending job was pruned (must never happen)")
		}
		if _, ok := store.jobs["old-done"]; ok {
			t.Errorf("old finished job should have been pruned")
		}
	})
}

// TestRunRetention_PrunesJobs ensures that the standard retention pass also
// prunes finished jobs older than the 30-day cutoff (D-17 combined behavior).
func TestRunRetention_PrunesJobs(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)
	clock := fixedClock{t: now}
	repoID := "repo-alpha"

	store := newFakeStore()
	dst := newFakeDst()
	// 3 recent succeeded records (none pruned by count/age).
	baseTime := now.Add(-10 * time.Hour)
	seedSuccessRecords(store, repoID, 3, baseTime, time.Hour)

	// 2 old finished jobs (> 30 days) and 1 recent.
	oldT := now.Add(-40 * 24 * time.Hour)
	recentT := now.Add(-5 * 24 * time.Hour)
	addJob(store, "oldjob-1", repoID, models.BackupStatusSucceeded, &oldT)
	addJob(store, "oldjob-2", repoID, models.BackupStatusSucceeded, &oldT)
	addJob(store, "recent-1", repoID, models.BackupStatusSucceeded, &recentT)

	keep := 5 // keepCount high enough to keep all records.
	repo := &models.BackupRepo{ID: repoID, KeepCount: &keep, KeepAgeDays: nil}

	report, err := RunRetention(ctx, repo, dst, store, clock)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.JobsPruned != 2 {
		t.Errorf("expected JobsPruned=2, got %d", report.JobsPruned)
	}
}
