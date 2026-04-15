package models

import (
	"errors"
	"testing"
)

func TestBackupRepoTableName(t *testing.T) {
	if (BackupRepo{}).TableName() != "backup_repos" {
		t.Errorf("expected table name 'backup_repos', got %q", (BackupRepo{}).TableName())
	}
}

func TestBackupRecordTableName(t *testing.T) {
	if (BackupRecord{}).TableName() != "backup_records" {
		t.Errorf("expected table name 'backup_records', got %q", (BackupRecord{}).TableName())
	}
}

func TestBackupJobTableName(t *testing.T) {
	if (BackupJob{}).TableName() != "backup_jobs" {
		t.Errorf("expected table name 'backup_jobs', got %q", (BackupJob{}).TableName())
	}
}

func TestBackupStatusConstants(t *testing.T) {
	cases := map[BackupStatus]string{
		BackupStatusPending:     "pending",
		BackupStatusRunning:     "running",
		BackupStatusSucceeded:   "succeeded",
		BackupStatusFailed:      "failed",
		BackupStatusInterrupted: "interrupted",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("expected %q, got %q", want, got)
		}
	}
}

func TestBackupJobKindConstants(t *testing.T) {
	if string(BackupJobKindBackup) != "backup" {
		t.Errorf("expected 'backup', got %q", BackupJobKindBackup)
	}
	if string(BackupJobKindRestore) != "restore" {
		t.Errorf("expected 'restore', got %q", BackupJobKindRestore)
	}
}

func TestBackupRepoKindConstants(t *testing.T) {
	if string(BackupRepoKindLocal) != "local" {
		t.Errorf("expected 'local', got %q", BackupRepoKindLocal)
	}
	if string(BackupRepoKindS3) != "s3" {
		t.Errorf("expected 's3', got %q", BackupRepoKindS3)
	}
}

func TestBackupRepoGetSetConfig(t *testing.T) {
	r := &BackupRepo{}

	cfg := map[string]any{"bucket": "my-bucket", "region": "us-east-1"}
	if err := r.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig failed: %v", err)
	}
	if r.Config == "" {
		t.Error("expected Config to be non-empty after SetConfig")
	}

	got, err := r.GetConfig()
	if err != nil {
		t.Fatalf("GetConfig failed: %v", err)
	}
	if got["bucket"] != "my-bucket" {
		t.Errorf("expected bucket 'my-bucket', got %v", got["bucket"])
	}
	if got["region"] != "us-east-1" {
		t.Errorf("expected region 'us-east-1', got %v", got["region"])
	}
}

func TestBackupRepoEmptyGetConfig(t *testing.T) {
	r := &BackupRepo{}
	got, err := r.GetConfig()
	if err != nil {
		t.Fatalf("GetConfig failed: %v", err)
	}
	if got == nil {
		t.Error("expected non-nil map from empty GetConfig")
	}
}

func TestBackupRepoGetConfigCached(t *testing.T) {
	r := &BackupRepo{}
	if err := r.SetConfig(map[string]any{"path": "/backups"}); err != nil {
		t.Fatalf("SetConfig failed: %v", err)
	}

	// First call populates ParsedConfig (it's already set by SetConfig).
	first, err := r.GetConfig()
	if err != nil {
		t.Fatalf("first GetConfig failed: %v", err)
	}
	if r.ParsedConfig == nil {
		t.Fatal("expected ParsedConfig cache populated after SetConfig")
	}

	// Corrupt the serialized form; cached ParsedConfig should win.
	r.Config = "not-json"
	second, err := r.GetConfig()
	if err != nil {
		t.Fatalf("second GetConfig (cached) failed: %v", err)
	}
	if second["path"] != first["path"] {
		t.Errorf("expected cached value to match; first=%v second=%v", first, second)
	}
}

func TestBackupSentinelsDistinct(t *testing.T) {
	sentinels := []error{
		ErrBackupRepoNotFound,
		ErrDuplicateBackupRepo,
		ErrBackupRepoInUse,
		ErrBackupRecordNotFound,
		ErrDuplicateBackupRecord,
		ErrBackupJobNotFound,
		ErrDuplicateBackupJob,
	}
	for _, s := range sentinels {
		if s == nil {
			t.Error("sentinel must not be nil")
		}
	}
	// Pairwise distinct under errors.Is.
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("sentinels %d and %d must be distinct under errors.Is", i, j)
			}
		}
	}
}

func TestAllModelsIncludesBackup(t *testing.T) {
	var hasRepo, hasRecord, hasJob bool
	for _, m := range AllModels() {
		switch m.(type) {
		case *BackupRepo:
			hasRepo = true
		case *BackupRecord:
			hasRecord = true
		case *BackupJob:
			hasJob = true
		}
	}
	if !hasRepo {
		t.Error("AllModels missing *BackupRepo")
	}
	if !hasRecord {
		t.Error("AllModels missing *BackupRecord")
	}
	if !hasJob {
		t.Error("AllModels missing *BackupJob")
	}
}
