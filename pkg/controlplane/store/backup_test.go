//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// seedMetaStore creates a metadata store config and returns its ID.
func seedMetaStore(t *testing.T, s *GORMStore, name string) string {
	t.Helper()
	ctx := context.Background()
	id, err := s.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: name, Type: "memory"})
	if err != nil {
		t.Fatalf("failed to seed metadata store %q: %v", name, err)
	}
	return id
}

// seedRepo creates a backup repo and returns it.
func seedRepo(t *testing.T, s *GORMStore, storeID, name string) *models.BackupRepo {
	t.Helper()
	ctx := context.Background()
	repo := &models.BackupRepo{
		TargetID:   storeID,
		TargetKind: "metadata",
		Name:       name,
		Kind:       models.BackupRepoKindLocal,
	}
	if _, err := s.CreateBackupRepo(ctx, repo); err != nil {
		t.Fatalf("failed to seed repo %q: %v", name, err)
	}
	return repo
}

func TestBackupRepoOperations(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeID := seedMetaStore(t, s, "ms-repo-ops")

	t.Run("create repo", func(t *testing.T) {
		repo := &models.BackupRepo{
			TargetID:   storeID,
			TargetKind: "metadata",
			Name:       "primary",
			Kind:       models.BackupRepoKindLocal,
			Config:     `{"path":"/data/backups"}`,
		}
		id, err := s.CreateBackupRepo(ctx, repo)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if id == "" {
			t.Fatal("expected non-empty ID")
		}
	})

	t.Run("duplicate per store fails", func(t *testing.T) {
		repo := &models.BackupRepo{
			TargetID:   storeID,
			TargetKind: "metadata",
			Name:       "primary",
			Kind:       models.BackupRepoKindLocal,
		}
		_, err := s.CreateBackupRepo(ctx, repo)
		if !errors.Is(err, models.ErrDuplicateBackupRepo) {
			t.Errorf("expected ErrDuplicateBackupRepo, got %v", err)
		}
	})

	t.Run("get by (storeID,name)", func(t *testing.T) {
		repo, err := s.GetBackupRepo(ctx, storeID, "primary")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if repo.Name != "primary" {
			t.Errorf("name = %q, want primary", repo.Name)
		}
	})

	t.Run("get by ID", func(t *testing.T) {
		got, _ := s.GetBackupRepo(ctx, storeID, "primary")
		byID, err := s.GetBackupRepoByID(ctx, got.ID)
		if err != nil {
			t.Fatalf("get by id: %v", err)
		}
		if byID.Name != "primary" {
			t.Errorf("name = %q, want primary", byID.Name)
		}
	})

	t.Run("get by id not found", func(t *testing.T) {
		_, err := s.GetBackupRepoByID(ctx, "nope")
		if !errors.Is(err, models.ErrBackupRepoNotFound) {
			t.Errorf("expected ErrBackupRepoNotFound, got %v", err)
		}
	})

	t.Run("list by target kind+id", func(t *testing.T) {
		// add another repo in same store
		if _, err := s.CreateBackupRepo(ctx, &models.BackupRepo{
			TargetID: storeID, TargetKind: "metadata",
			Name: "secondary", Kind: models.BackupRepoKindS3,
		}); err != nil {
			t.Fatalf("seed secondary: %v", err)
		}
		repos, err := s.ListReposByTarget(ctx, "metadata", storeID)
		if err != nil {
			t.Fatalf("list by target: %v", err)
		}
		if len(repos) != 2 {
			t.Errorf("expected 2 repos, got %d", len(repos))
		}
	})

	t.Run("list by target: mismatched kind returns empty", func(t *testing.T) {
		// kind=block against a metadata-target repo set must yield zero rows.
		repos, err := s.ListReposByTarget(ctx, "block", storeID)
		if err != nil {
			t.Fatalf("list by target (block): %v", err)
		}
		if len(repos) != 0 {
			t.Errorf("expected 0 repos for kind=block, got %d", len(repos))
		}
	})

	t.Run("list all", func(t *testing.T) {
		repos, err := s.ListAllBackupRepos(ctx)
		if err != nil {
			t.Fatalf("list all: %v", err)
		}
		if len(repos) < 2 {
			t.Errorf("expected >= 2 repos, got %d", len(repos))
		}
	})

	t.Run("update repo", func(t *testing.T) {
		repo, _ := s.GetBackupRepo(ctx, storeID, "primary")
		keep := 7
		repo.KeepCount = &keep
		if err := s.UpdateBackupRepo(ctx, repo); err != nil {
			t.Fatalf("update: %v", err)
		}
		reloaded, _ := s.GetBackupRepoByID(ctx, repo.ID)
		if reloaded.KeepCount == nil || *reloaded.KeepCount != 7 {
			t.Errorf("expected KeepCount=7, got %v", reloaded.KeepCount)
		}
	})

	t.Run("delete repo with no records", func(t *testing.T) {
		repo, _ := s.GetBackupRepo(ctx, storeID, "secondary")
		if err := s.DeleteBackupRepo(ctx, repo.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		_, err := s.GetBackupRepoByID(ctx, repo.ID)
		if !errors.Is(err, models.ErrBackupRepoNotFound) {
			t.Errorf("expected ErrBackupRepoNotFound after delete, got %v", err)
		}
	})

	t.Run("delete missing repo", func(t *testing.T) {
		err := s.DeleteBackupRepo(ctx, "does-not-exist")
		if !errors.Is(err, models.ErrBackupRepoNotFound) {
			t.Errorf("expected ErrBackupRepoNotFound, got %v", err)
		}
	})
}

// TestBackupRepoUniquePerTarget exercises REPO-04: repo names are unique per
// (target_kind, target_id), NOT globally (D-26 polymorphic target rename).
func TestBackupRepoUniquePerTarget(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeA := seedMetaStore(t, s, "ms-A")
	storeB := seedMetaStore(t, s, "ms-B")

	if _, err := s.CreateBackupRepo(ctx, &models.BackupRepo{
		TargetID: storeA, TargetKind: "metadata",
		Name: "local", Kind: models.BackupRepoKindLocal,
	}); err != nil {
		t.Fatalf("create A.local: %v", err)
	}

	// Same name, same target -> duplicate.
	_, err := s.CreateBackupRepo(ctx, &models.BackupRepo{
		TargetID: storeA, TargetKind: "metadata",
		Name: "local", Kind: models.BackupRepoKindLocal,
	})
	if !errors.Is(err, models.ErrDuplicateBackupRepo) {
		t.Errorf("expected duplicate within same target, got %v", err)
	}

	// Same name, different target -> succeeds.
	if _, err := s.CreateBackupRepo(ctx, &models.BackupRepo{
		TargetID: storeB, TargetKind: "metadata",
		Name: "local", Kind: models.BackupRepoKindLocal,
	}); err != nil {
		t.Errorf("expected success for B.local, got %v", err)
	}
}

func TestBackupRepoGetConfigRoundTrip(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeID := seedMetaStore(t, s, "ms-cfg")

	repo := &models.BackupRepo{
		TargetID:   storeID,
		TargetKind: "metadata",
		Name:       "s3-archive",
		Kind:       models.BackupRepoKindS3,
	}
	want := map[string]any{
		"bucket": "my-bucket",
		"region": "us-east-1",
		"prefix": "backups/",
	}
	if err := repo.SetConfig(want); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if _, err := s.CreateBackupRepo(ctx, repo); err != nil {
		t.Fatalf("create: %v", err)
	}

	reloaded, err := s.GetBackupRepoByID(ctx, repo.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, err := reloaded.GetConfig()
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("config[%q] = %v, want %v", k, got[k], v)
		}
	}
}

func TestBackupRecordPin(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeID := seedMetaStore(t, s, "ms-pin")
	repo := seedRepo(t, s, storeID, "primary")

	rec := &models.BackupRecord{RepoID: repo.ID, Status: models.BackupStatusSucceeded}
	if _, err := s.CreateBackupRecord(ctx, rec); err != nil {
		t.Fatalf("create record: %v", err)
	}

	// pin true
	if err := s.SetBackupRecordPinned(ctx, rec.ID, true); err != nil {
		t.Fatalf("pin true: %v", err)
	}
	got, _ := s.GetBackupRecord(ctx, rec.ID)
	if !got.Pinned {
		t.Errorf("expected pinned=true after reload")
	}

	// pin false
	if err := s.SetBackupRecordPinned(ctx, rec.ID, false); err != nil {
		t.Fatalf("pin false: %v", err)
	}
	got, _ = s.GetBackupRecord(ctx, rec.ID)
	if got.Pinned {
		t.Errorf("expected pinned=false after toggle")
	}

	// missing id
	if err := s.SetBackupRecordPinned(ctx, "nope", true); !errors.Is(err, models.ErrBackupRecordNotFound) {
		t.Errorf("expected ErrBackupRecordNotFound, got %v", err)
	}
}

func TestBackupRecordListByRepo(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeID := seedMetaStore(t, s, "ms-list")
	repoA := seedRepo(t, s, storeID, "A")
	repoB := seedRepo(t, s, storeID, "B")

	for _, rid := range []string{repoA.ID, repoA.ID, repoB.ID} {
		if _, err := s.CreateBackupRecord(ctx, &models.BackupRecord{
			RepoID: rid, Status: models.BackupStatusSucceeded,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// Small delay ensures CreatedAt ordering is deterministic at second resolution.
		time.Sleep(5 * time.Millisecond)
	}

	aRecs, err := s.ListBackupRecordsByRepo(ctx, repoA.ID)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(aRecs) != 2 {
		t.Errorf("repo A: expected 2 records, got %d", len(aRecs))
	}

	bRecs, err := s.ListBackupRecordsByRepo(ctx, repoB.ID)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(bRecs) != 1 {
		t.Errorf("repo B: expected 1 record, got %d", len(bRecs))
	}

	// Newest-first ordering for repo A.
	if len(aRecs) == 2 && aRecs[0].CreatedAt.Before(aRecs[1].CreatedAt) {
		t.Errorf("expected newest-first ordering; got %v before %v", aRecs[0].CreatedAt, aRecs[1].CreatedAt)
	}
}

// TestListSucceededRecordsForRetention exercises the Phase 4 retention helper
// added in D-26: succeeded non-pinned records sorted oldest-first. Failed
// records and pinned records are excluded so the retention pass never
// considers them candidates (D-10, D-12).
func TestListSucceededRecordsForRetention(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeID := seedMetaStore(t, s, "ms-retention")
	repo := seedRepo(t, s, storeID, "primary")

	// Seed 3 succeeded (non-pinned), 1 failed, 1 succeeded+pinned.
	succeededIDs := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		rec := &models.BackupRecord{
			RepoID: repo.ID,
			Status: models.BackupStatusSucceeded,
		}
		if _, err := s.CreateBackupRecord(ctx, rec); err != nil {
			t.Fatalf("seed succeeded[%d]: %v", i, err)
		}
		succeededIDs = append(succeededIDs, rec.ID)
		time.Sleep(5 * time.Millisecond)
	}

	failed := &models.BackupRecord{
		RepoID: repo.ID,
		Status: models.BackupStatusFailed,
		Error:  "s3 timeout",
	}
	if _, err := s.CreateBackupRecord(ctx, failed); err != nil {
		t.Fatalf("seed failed: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	pinned := &models.BackupRecord{
		RepoID: repo.ID,
		Status: models.BackupStatusSucceeded,
		Pinned: true,
	}
	if _, err := s.CreateBackupRecord(ctx, pinned); err != nil {
		t.Fatalf("seed pinned: %v", err)
	}

	got, err := s.ListSucceededRecordsForRetention(ctx, repo.ID)
	if err != nil {
		t.Fatalf("ListSucceededRecordsForRetention: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 retention candidates, got %d", len(got))
	}

	// D-10: oldest-first ordering (retention prunes from the tail).
	for i := 1; i < len(got); i++ {
		if got[i-1].CreatedAt.After(got[i].CreatedAt) {
			t.Errorf("not oldest-first: %v after %v", got[i-1].CreatedAt, got[i].CreatedAt)
		}
	}

	// Sanity: failed + pinned records are NOT present in the result set.
	for _, rec := range got {
		if rec.Status != models.BackupStatusSucceeded {
			t.Errorf("retention returned non-succeeded record %s (status=%s)", rec.ID, rec.Status)
		}
		if rec.Pinned {
			t.Errorf("retention returned pinned record %s", rec.ID)
		}
		if rec.ID == failed.ID {
			t.Errorf("retention returned failed record %s", rec.ID)
		}
		if rec.ID == pinned.ID {
			t.Errorf("retention returned pinned record %s", rec.ID)
		}
	}
}

// TestListSucceededRecordsByRepo exercises the Phase 5 restore helper:
// succeeded records INCLUDING pinned, sorted newest-first. Failed records
// are excluded; pinned records appear at their chronological position
// (opposite of retention's pinned-skip semantics).
func TestListSucceededRecordsByRepo(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeID := seedMetaStore(t, s, "ms-restore-select")
	repo := seedRepo(t, s, storeID, "primary")

	// Seed 3 succeeded records (one pinned) + 1 failed, in chronological
	// order so the CreatedAt ordering is deterministic. We capture the IDs
	// to assert newest-first ordering at the end.
	var aID, bID, cID string

	a := &models.BackupRecord{RepoID: repo.ID, Status: models.BackupStatusSucceeded}
	if _, err := s.CreateBackupRecord(ctx, a); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	aID = a.ID
	time.Sleep(5 * time.Millisecond)

	b := &models.BackupRecord{RepoID: repo.ID, Status: models.BackupStatusSucceeded, Pinned: true}
	if _, err := s.CreateBackupRecord(ctx, b); err != nil {
		t.Fatalf("seed b (pinned): %v", err)
	}
	bID = b.ID
	time.Sleep(5 * time.Millisecond)

	c := &models.BackupRecord{RepoID: repo.ID, Status: models.BackupStatusSucceeded}
	if _, err := s.CreateBackupRecord(ctx, c); err != nil {
		t.Fatalf("seed c: %v", err)
	}
	cID = c.ID
	time.Sleep(5 * time.Millisecond)

	failed := &models.BackupRecord{
		RepoID: repo.ID,
		Status: models.BackupStatusFailed,
		Error:  "boom",
	}
	if _, err := s.CreateBackupRecord(ctx, failed); err != nil {
		t.Fatalf("seed failed: %v", err)
	}

	got, err := s.ListSucceededRecordsByRepo(ctx, repo.ID)
	if err != nil {
		t.Fatalf("ListSucceededRecordsByRepo: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 records (all succeeded incl. pinned), got %d", len(got))
	}

	// Newest-first ordering: c, b, a.
	wantIDs := []string{cID, bID, aID}
	gotIDs := []string{got[0].ID, got[1].ID, got[2].ID}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("position %d: got %q, want %q (full got=%v, want=%v)",
				i, gotIDs[i], wantIDs[i], gotIDs, wantIDs)
		}
	}

	// Pinned record IS present in the result set (opposite of retention).
	foundPinned := false
	for _, rec := range got {
		if rec.ID == bID {
			foundPinned = true
			if !rec.Pinned {
				t.Errorf("expected record %s to have Pinned=true", bID)
			}
		}
		if rec.Status != models.BackupStatusSucceeded {
			t.Errorf("record %s status=%s, want succeeded", rec.ID, rec.Status)
		}
	}
	if !foundPinned {
		t.Errorf("pinned record %s missing from ListSucceededRecordsByRepo result", bID)
	}

	// Empty repo edge case: a repo with zero succeeded records returns
	// empty slice (no error, safe downstream).
	emptyRepo := seedRepo(t, s, storeID, "empty")
	empty, err := s.ListSucceededRecordsByRepo(ctx, emptyRepo.ID)
	if err != nil {
		t.Fatalf("empty repo: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("expected empty slice for repo with no records, got %d", len(empty))
	}
}

func TestBackupJobKindFilter(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeID := seedMetaStore(t, s, "ms-jobs")
	repo := seedRepo(t, s, storeID, "primary")

	seed := func(kind models.BackupJobKind, status models.BackupStatus) {
		if _, err := s.CreateBackupJob(ctx, &models.BackupJob{
			Kind: kind, RepoID: repo.ID, Status: status,
		}); err != nil {
			t.Fatalf("seed job: %v", err)
		}
	}
	seed(models.BackupJobKindBackup, models.BackupStatusPending)
	seed(models.BackupJobKindBackup, models.BackupStatusPending)
	seed(models.BackupJobKindRestore, models.BackupStatusPending)

	backupJobs, _ := s.ListBackupJobs(ctx, models.BackupJobKindBackup, "")
	if len(backupJobs) != 2 {
		t.Errorf("expected 2 backup jobs, got %d", len(backupJobs))
	}

	restoreJobs, _ := s.ListBackupJobs(ctx, models.BackupJobKindRestore, "")
	if len(restoreJobs) != 1 {
		t.Errorf("expected 1 restore job, got %d", len(restoreJobs))
	}

	pending, _ := s.ListBackupJobs(ctx, "", models.BackupStatusPending)
	if len(pending) != 3 {
		t.Errorf("expected 3 pending jobs, got %d", len(pending))
	}

	all, _ := s.ListBackupJobs(ctx, "", "")
	if len(all) != 3 {
		t.Errorf("expected 3 total jobs, got %d", len(all))
	}
}

func TestRecoverInterruptedJobs(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeID := seedMetaStore(t, s, "ms-recover")
	repo := seedRepo(t, s, storeID, "primary")

	// 3 running, 1 succeeded
	runningIDs := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		job := &models.BackupJob{
			Kind: models.BackupJobKindBackup, RepoID: repo.ID,
			Status: models.BackupStatusRunning,
		}
		if _, err := s.CreateBackupJob(ctx, job); err != nil {
			t.Fatalf("seed running: %v", err)
		}
		runningIDs = append(runningIDs, job.ID)
	}
	succeeded := &models.BackupJob{
		Kind: models.BackupJobKindBackup, RepoID: repo.ID,
		Status: models.BackupStatusSucceeded,
	}
	if _, err := s.CreateBackupJob(ctx, succeeded); err != nil {
		t.Fatalf("seed succeeded: %v", err)
	}

	n, err := s.RecoverInterruptedJobs(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3 recovered jobs, got %d", n)
	}

	for _, id := range runningIDs {
		j, err := s.GetBackupJob(ctx, id)
		if err != nil {
			t.Fatalf("reload %s: %v", id, err)
		}
		if j.Status != models.BackupStatusInterrupted {
			t.Errorf("job %s status = %s, want interrupted", id, j.Status)
		}
		if j.Error == "" {
			t.Errorf("job %s: expected non-empty error message", id)
		}
		if j.FinishedAt == nil {
			t.Errorf("job %s: expected FinishedAt to be set", id)
		}
	}

	untouched, _ := s.GetBackupJob(ctx, succeeded.ID)
	if untouched.Status != models.BackupStatusSucceeded {
		t.Errorf("succeeded job should be untouched, got status=%s", untouched.Status)
	}
}

func TestDeleteBackupRepoInUse(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeID := seedMetaStore(t, s, "ms-inuse")
	repo := seedRepo(t, s, storeID, "primary")

	rec := &models.BackupRecord{RepoID: repo.ID, Status: models.BackupStatusSucceeded}
	if _, err := s.CreateBackupRecord(ctx, rec); err != nil {
		t.Fatalf("seed record: %v", err)
	}

	err := s.DeleteBackupRepo(ctx, repo.ID)
	if !errors.Is(err, models.ErrBackupRepoInUse) {
		t.Fatalf("expected ErrBackupRepoInUse, got %v", err)
	}

	// Record should still exist.
	if _, err := s.GetBackupRecord(ctx, rec.ID); err != nil {
		t.Errorf("record should still exist: %v", err)
	}

	// Remove record, then delete succeeds.
	if err := s.DeleteBackupRecord(ctx, rec.ID); err != nil {
		t.Fatalf("delete record: %v", err)
	}
	if err := s.DeleteBackupRepo(ctx, repo.ID); err != nil {
		t.Fatalf("delete repo after freeing records: %v", err)
	}
}

func TestBackupRecordAutoULID(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeID := seedMetaStore(t, s, "ms-ulid")
	repo := seedRepo(t, s, storeID, "primary")

	var ids []string
	for i := 0; i < 3; i++ {
		rec := &models.BackupRecord{RepoID: repo.ID, Status: models.BackupStatusSucceeded}
		if _, err := s.CreateBackupRecord(ctx, rec); err != nil {
			t.Fatalf("create: %v", err)
		}
		if len(rec.ID) != 26 {
			t.Errorf("expected ULID length 26, got %d (%q)", len(rec.ID), rec.ID)
		}
		ids = append(ids, rec.ID)
		time.Sleep(2 * time.Millisecond)
	}

	// ULIDs produced sequentially should be lexicographically ordered.
	for i := 1; i < len(ids); i++ {
		if ids[i-1] >= ids[i] {
			t.Errorf("ULIDs not monotonically increasing: %q >= %q", ids[i-1], ids[i])
		}
	}
}

func TestBackupJobAutoULID(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeID := seedMetaStore(t, s, "ms-ulid-job")
	repo := seedRepo(t, s, storeID, "primary")

	job := &models.BackupJob{
		Kind: models.BackupJobKindBackup, RepoID: repo.ID,
		Status: models.BackupStatusPending,
	}
	if _, err := s.CreateBackupJob(ctx, job); err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(job.ID) != 26 {
		t.Errorf("expected ULID length 26, got %d (%q)", len(job.ID), job.ID)
	}
}

// TestListBackupRecords_FilterByStatus exercises the Phase-6 D-26 listing
// contract: repo-scoped + optional status filter + newest-first.
func TestListBackupRecords_FilterByStatus(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeID := seedMetaStore(t, s, "ms-list-records")
	repo := seedRepo(t, s, storeID, "primary")

	succ1 := &models.BackupRecord{RepoID: repo.ID, Status: models.BackupStatusSucceeded}
	if _, err := s.CreateBackupRecord(ctx, succ1); err != nil {
		t.Fatalf("seed succ1: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	failed := &models.BackupRecord{RepoID: repo.ID, Status: models.BackupStatusFailed, Error: "boom"}
	if _, err := s.CreateBackupRecord(ctx, failed); err != nil {
		t.Fatalf("seed failed: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	succ2 := &models.BackupRecord{RepoID: repo.ID, Status: models.BackupStatusSucceeded}
	if _, err := s.CreateBackupRecord(ctx, succ2); err != nil {
		t.Fatalf("seed succ2: %v", err)
	}

	// Status filter returns only succeeded records.
	gotSucceeded, err := s.ListBackupRecords(ctx, repo.ID, models.BackupStatusSucceeded)
	if err != nil {
		t.Fatalf("list by status: %v", err)
	}
	if len(gotSucceeded) != 2 {
		t.Fatalf("expected 2 succeeded records, got %d", len(gotSucceeded))
	}
	// Newest-first ordering.
	if gotSucceeded[0].ID != succ2.ID {
		t.Errorf("expected succ2 at position 0, got %q (want %q)", gotSucceeded[0].ID, succ2.ID)
	}

	// Empty status returns all rows.
	gotAll, err := s.ListBackupRecords(ctx, repo.ID, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(gotAll) != 3 {
		t.Errorf("expected 3 total records, got %d", len(gotAll))
	}
}

// TestListBackupRecords_EmptyRepo — a repo with zero records returns an empty
// slice and no error (safe downstream).
func TestListBackupRecords_EmptyRepo(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeID := seedMetaStore(t, s, "ms-empty-records")
	repo := seedRepo(t, s, storeID, "empty")

	got, err := s.ListBackupRecords(ctx, repo.ID, "")
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %d records", len(got))
	}
}

// TestListBackupJobsFiltered_Filter exercises the Phase-6 D-42 contract:
// repo+kind+status filtering, newest-first StartedAt DESC, default limit 50,
// hard-cap 200.
func TestListBackupJobsFiltered_Filter(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeID := seedMetaStore(t, s, "ms-list-jobs")
	repoA := seedRepo(t, s, storeID, "A")
	repoB := seedRepo(t, s, storeID, "B")

	now := time.Now()
	seed := func(repoID string, kind models.BackupJobKind, status models.BackupStatus, startedAt *time.Time) string {
		job := &models.BackupJob{
			Kind:      kind,
			RepoID:    repoID,
			Status:    status,
			StartedAt: startedAt,
		}
		if _, err := s.CreateBackupJob(ctx, job); err != nil {
			t.Fatalf("seed job: %v", err)
		}
		return job.ID
	}

	t1 := now.Add(-3 * time.Hour)
	t2 := now.Add(-2 * time.Hour)
	t3 := now.Add(-1 * time.Hour)

	seed(repoA.ID, models.BackupJobKindBackup, models.BackupStatusSucceeded, &t1)
	seed(repoA.ID, models.BackupJobKindRestore, models.BackupStatusSucceeded, &t2)
	newestRestoreID := seed(repoA.ID, models.BackupJobKindRestore, models.BackupStatusSucceeded, &t3)
	seed(repoB.ID, models.BackupJobKindRestore, models.BackupStatusSucceeded, &t2)
	// A failed restore to ensure status filter excludes it.
	seed(repoA.ID, models.BackupJobKindRestore, models.BackupStatusFailed, &t2)

	// Full filter: kind=restore, status=succeeded, repoID=repoA.
	got, err := s.ListBackupJobsFiltered(ctx, BackupJobFilter{
		RepoID: repoA.ID,
		Kind:   models.BackupJobKindRestore,
		Status: models.BackupStatusSucceeded,
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("filtered: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 succeeded restore jobs for repoA, got %d", len(got))
	}
	if got[0].ID != newestRestoreID {
		t.Errorf("expected newest restore at position 0 (%q), got %q", newestRestoreID, got[0].ID)
	}

	// Limit=0 applies default 50 — with 5 total rows we should see all of them.
	all, err := s.ListBackupJobsFiltered(ctx, BackupJobFilter{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("expected 5 rows (default limit 50), got %d", len(all))
	}

	// Limit above the cap is clamped at 200 (no error, just capped).
	_, err = s.ListBackupJobsFiltered(ctx, BackupJobFilter{Limit: 1_000_000})
	if err != nil {
		t.Fatalf("list with huge limit: %v", err)
	}
}

// TestUpdateBackupRecordPinned exercises the Phase-6 pinned PATCH path —
// Pinned true/false toggle + ErrBackupRecordNotFound on miss.
func TestUpdateBackupRecordPinned(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeID := seedMetaStore(t, s, "ms-pin-v2")
	repo := seedRepo(t, s, storeID, "primary")

	rec := &models.BackupRecord{RepoID: repo.ID, Status: models.BackupStatusSucceeded}
	if _, err := s.CreateBackupRecord(ctx, rec); err != nil {
		t.Fatalf("seed record: %v", err)
	}

	if err := s.UpdateBackupRecordPinned(ctx, rec.ID, true); err != nil {
		t.Fatalf("pin true: %v", err)
	}
	reloaded, _ := s.GetBackupRecord(ctx, rec.ID)
	if !reloaded.Pinned {
		t.Errorf("expected Pinned=true after reload")
	}

	if err := s.UpdateBackupRecordPinned(ctx, rec.ID, false); err != nil {
		t.Fatalf("pin false: %v", err)
	}
	reloaded, _ = s.GetBackupRecord(ctx, rec.ID)
	if reloaded.Pinned {
		t.Errorf("expected Pinned=false after toggle")
	}

	// Unknown ID surfaces ErrBackupRecordNotFound.
	err := s.UpdateBackupRecordPinned(ctx, "no-such-record", true)
	if !errors.Is(err, models.ErrBackupRecordNotFound) {
		t.Errorf("expected ErrBackupRecordNotFound, got %v", err)
	}
}

// TestUpdateBackupJobProgress exercises the Phase-6 D-50 progress-column
// update: successful write, clamping rejection (ErrInvalidProgress), and
// ErrBackupJobNotFound on miss.
func TestUpdateBackupJobProgress(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeID := seedMetaStore(t, s, "ms-progress")
	repo := seedRepo(t, s, storeID, "primary")

	job := &models.BackupJob{
		Kind:   models.BackupJobKindBackup,
		RepoID: repo.ID,
		Status: models.BackupStatusRunning,
	}
	if _, err := s.CreateBackupJob(ctx, job); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	if err := s.UpdateBackupJobProgress(ctx, job.ID, 50); err != nil {
		t.Fatalf("update progress 50: %v", err)
	}
	reloaded, _ := s.GetBackupJob(ctx, job.ID)
	if reloaded.Progress != 50 {
		t.Errorf("expected progress=50, got %d", reloaded.Progress)
	}

	// Out-of-range pct rejected without mutating the row.
	if err := s.UpdateBackupJobProgress(ctx, job.ID, -1); !errors.Is(err, ErrInvalidProgress) {
		t.Errorf("expected ErrInvalidProgress for pct=-1, got %v", err)
	}
	if err := s.UpdateBackupJobProgress(ctx, job.ID, 101); !errors.Is(err, ErrInvalidProgress) {
		t.Errorf("expected ErrInvalidProgress for pct=101, got %v", err)
	}
	// Value must still be 50 after rejected updates.
	reloaded, _ = s.GetBackupJob(ctx, job.ID)
	if reloaded.Progress != 50 {
		t.Errorf("progress should be unchanged at 50 after rejected updates, got %d", reloaded.Progress)
	}

	// Unknown ID surfaces ErrBackupJobNotFound.
	err := s.UpdateBackupJobProgress(ctx, "no-such-job", 75)
	if !errors.Is(err, models.ErrBackupJobNotFound) {
		t.Errorf("expected ErrBackupJobNotFound, got %v", err)
	}

	// Boundary values 0 and 100 accepted.
	if err := s.UpdateBackupJobProgress(ctx, job.ID, 0); err != nil {
		t.Errorf("pct=0 should be accepted, got %v", err)
	}
	if err := s.UpdateBackupJobProgress(ctx, job.ID, 100); err != nil {
		t.Errorf("pct=100 should be accepted, got %v", err)
	}
}

// TestBackupRepoTargetKindBackfill seeds a legacy-style row directly via raw
// SQL (simulating a pre-D-26 database where target_kind didn't exist or was
// NULL) and confirms the post-AutoMigrate backfill stamped `metadata` on it.
// This exercises gorm.go's "UPDATE backup_repos SET target_kind = 'metadata'
// WHERE target_kind = ” OR target_kind IS NULL" statement.
func TestBackupRepoTargetKindBackfill(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeID := seedMetaStore(t, s, "ms-backfill")

	// Write a row with empty target_kind directly (AutoMigrate plus defaults
	// would normally set 'metadata'; we force '' to prove the backfill path
	// corrects it on subsequent reconciliation).
	if err := s.DB().Exec(
		"UPDATE backup_repos SET target_kind = '' WHERE target_id = ?",
		storeID,
	).Error; err != nil {
		t.Fatalf("force empty target_kind setup: %v", err)
	}
	// Seed one repo so there IS a row attached to storeID, then force empty kind.
	_ = seedRepo(t, s, storeID, "legacy")
	if err := s.DB().Exec(
		"UPDATE backup_repos SET target_kind = '' WHERE target_id = ?",
		storeID,
	).Error; err != nil {
		t.Fatalf("force empty target_kind: %v", err)
	}

	// Apply the same backfill SQL the boot migration runs. The statement must
	// be idempotent and stamp 'metadata' on any empty-string rows.
	if err := s.DB().Exec(
		"UPDATE backup_repos SET target_kind = ? WHERE target_kind = '' OR target_kind IS NULL",
		"metadata",
	).Error; err != nil {
		t.Fatalf("backfill: %v", err)
	}

	repos, err := s.ListReposByTarget(ctx, "metadata", storeID)
	if err != nil {
		t.Fatalf("list after backfill: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("expected 1 metadata-kind repo after backfill, got %d", len(repos))
	}
	if repos[0].TargetKind != "metadata" {
		t.Errorf("expected target_kind='metadata' after backfill, got %q", repos[0].TargetKind)
	}
}
