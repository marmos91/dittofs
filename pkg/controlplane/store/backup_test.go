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
		MetadataStoreID: storeID,
		Name:            name,
		Kind:            models.BackupRepoKindLocal,
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
			MetadataStoreID: storeID,
			Name:            "primary",
			Kind:            models.BackupRepoKindLocal,
			Config:          `{"path":"/data/backups"}`,
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
			MetadataStoreID: storeID,
			Name:            "primary",
			Kind:            models.BackupRepoKindLocal,
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

	t.Run("list by store", func(t *testing.T) {
		// add another repo in same store
		if _, err := s.CreateBackupRepo(ctx, &models.BackupRepo{
			MetadataStoreID: storeID, Name: "secondary", Kind: models.BackupRepoKindS3,
		}); err != nil {
			t.Fatalf("seed secondary: %v", err)
		}
		repos, err := s.ListBackupReposByStore(ctx, storeID)
		if err != nil {
			t.Fatalf("list by store: %v", err)
		}
		if len(repos) != 2 {
			t.Errorf("expected 2 repos, got %d", len(repos))
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

// TestBackupRepoUniquePerStore exercises REPO-04: repo names are unique
// per metadata store, NOT globally.
func TestBackupRepoUniquePerStore(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	storeA := seedMetaStore(t, s, "ms-A")
	storeB := seedMetaStore(t, s, "ms-B")

	if _, err := s.CreateBackupRepo(ctx, &models.BackupRepo{
		MetadataStoreID: storeA, Name: "local", Kind: models.BackupRepoKindLocal,
	}); err != nil {
		t.Fatalf("create A.local: %v", err)
	}

	// Same name, same store -> duplicate.
	_, err := s.CreateBackupRepo(ctx, &models.BackupRepo{
		MetadataStoreID: storeA, Name: "local", Kind: models.BackupRepoKindLocal,
	})
	if !errors.Is(err, models.ErrDuplicateBackupRepo) {
		t.Errorf("expected duplicate within same store, got %v", err)
	}

	// Same name, different store -> succeeds.
	if _, err := s.CreateBackupRepo(ctx, &models.BackupRepo{
		MetadataStoreID: storeB, Name: "local", Kind: models.BackupRepoKindLocal,
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
		MetadataStoreID: storeID,
		Name:            "s3-archive",
		Kind:            models.BackupRepoKindS3,
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
