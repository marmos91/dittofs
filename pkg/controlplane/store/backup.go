package store

// Tables are created by AutoMigrate(models.AllModels()...); see
// pkg/controlplane/store/gorm.go. No manual migration code is required in
// this file.

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ----- Repo operations -----

func (s *GORMStore) GetBackupRepo(ctx context.Context, storeID, name string) (*models.BackupRepo, error) {
	var repo models.BackupRepo
	if err := s.db.WithContext(ctx).
		Where("metadata_store_id = ? AND name = ?", storeID, name).
		First(&repo).Error; err != nil {
		return nil, convertNotFoundError(err, models.ErrBackupRepoNotFound)
	}
	return &repo, nil
}

func (s *GORMStore) GetBackupRepoByID(ctx context.Context, id string) (*models.BackupRepo, error) {
	return getByField[models.BackupRepo](s.db, ctx, "id", id, models.ErrBackupRepoNotFound)
}

func (s *GORMStore) ListBackupReposByStore(ctx context.Context, storeID string) ([]*models.BackupRepo, error) {
	var results []*models.BackupRepo
	if err := s.db.WithContext(ctx).
		Where("metadata_store_id = ?", storeID).
		Find(&results).Error; err != nil {
		return nil, err
	}
	return results, nil
}

func (s *GORMStore) ListAllBackupRepos(ctx context.Context) ([]*models.BackupRepo, error) {
	return listAll[models.BackupRepo](s.db, ctx)
}

func (s *GORMStore) CreateBackupRepo(ctx context.Context, repo *models.BackupRepo) (string, error) {
	// Materialize ParsedConfig into the Config JSON blob if the caller only
	// populated the parsed map (mirrors the BlockStoreConfig convention).
	if repo.Config == "" && len(repo.ParsedConfig) > 0 {
		if err := repo.SetConfig(repo.ParsedConfig); err != nil {
			return "", err
		}
	}
	if repo.ID == "" {
		repo.ID = uuid.New().String()
	}
	return createWithID(s.db, ctx, repo,
		func(r *models.BackupRepo, id string) { r.ID = id },
		repo.ID, models.ErrDuplicateBackupRepo)
}

func (s *GORMStore) UpdateBackupRepo(ctx context.Context, repo *models.BackupRepo) error {
	// Mirror CreateBackupRepo: materialize ParsedConfig into the Config JSON blob
	// if the caller populated the parsed map but left Config empty.
	if repo.Config == "" && len(repo.ParsedConfig) > 0 {
		if err := repo.SetConfig(repo.ParsedConfig); err != nil {
			return err
		}
	}
	result := s.db.WithContext(ctx).
		Model(&models.BackupRepo{}).
		Where("id = ?", repo.ID).
		Updates(map[string]any{
			"name":               repo.Name,
			"kind":               repo.Kind,
			"config":             repo.Config,
			"schedule":           repo.Schedule,
			"keep_count":         repo.KeepCount,
			"keep_age_days":      repo.KeepAgeDays,
			"encryption_enabled": repo.EncryptionEnabled,
			"encryption_key_ref": repo.EncryptionKeyRef,
		})
	if result.Error != nil {
		if isUniqueConstraintError(result.Error) {
			return models.ErrDuplicateBackupRepo
		}
		return result.Error
	}
	if result.RowsAffected == 0 {
		return models.ErrBackupRepoNotFound
	}
	return nil
}

func (s *GORMStore) DeleteBackupRepo(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var recordCount int64
		if err := tx.Model(&models.BackupRecord{}).
			Where("repo_id = ?", id).
			Count(&recordCount).Error; err != nil {
			return err
		}
		if recordCount > 0 {
			return models.ErrBackupRepoInUse
		}
		var activeJobCount int64
		if err := tx.Model(&models.BackupJob{}).
			Where("repo_id = ? AND status IN ?", id,
				[]models.BackupStatus{models.BackupStatusPending, models.BackupStatusRunning}).
			Count(&activeJobCount).Error; err != nil {
			return err
		}
		if activeJobCount > 0 {
			return models.ErrBackupRepoInUse
		}
		return deleteByField[models.BackupRepo](tx, ctx, "id", id, models.ErrBackupRepoNotFound)
	})
}

// ----- Record operations -----

func (s *GORMStore) GetBackupRecord(ctx context.Context, id string) (*models.BackupRecord, error) {
	return getByField[models.BackupRecord](s.db, ctx, "id", id, models.ErrBackupRecordNotFound)
}

func (s *GORMStore) ListBackupRecordsByRepo(ctx context.Context, repoID string) ([]*models.BackupRecord, error) {
	var results []*models.BackupRecord
	if err := s.db.WithContext(ctx).
		Where("repo_id = ?", repoID).
		Order("created_at DESC").
		Find(&results).Error; err != nil {
		return nil, err
	}
	return results, nil
}

func (s *GORMStore) CreateBackupRecord(ctx context.Context, rec *models.BackupRecord) (string, error) {
	if rec.ID == "" {
		rec.ID = ulid.Make().String()
	}
	return createWithID(s.db, ctx, rec,
		func(r *models.BackupRecord, id string) { r.ID = id },
		rec.ID, models.ErrDuplicateBackupRecord)
}

func (s *GORMStore) UpdateBackupRecord(ctx context.Context, rec *models.BackupRecord) error {
	result := s.db.WithContext(ctx).
		Model(&models.BackupRecord{}).
		Where("id = ?", rec.ID).
		Updates(map[string]any{
			"status":        rec.Status,
			"size_bytes":    rec.SizeBytes,
			"pinned":        rec.Pinned,
			"manifest_path": rec.ManifestPath,
			"sha256":        rec.SHA256,
			"error":         rec.Error,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return models.ErrBackupRecordNotFound
	}
	return nil
}

func (s *GORMStore) DeleteBackupRecord(ctx context.Context, id string) error {
	rec, err := s.GetBackupRecord(ctx, id)
	if err != nil {
		return err
	}
	if rec.Pinned {
		return models.ErrBackupRecordPinned
	}
	return deleteByField[models.BackupRecord](s.db, ctx, "id", id, models.ErrBackupRecordNotFound)
}

func (s *GORMStore) SetBackupRecordPinned(ctx context.Context, id string, pinned bool) error {
	result := s.db.WithContext(ctx).
		Model(&models.BackupRecord{}).
		Where("id = ?", id).
		Update("pinned", pinned)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return models.ErrBackupRecordNotFound
	}
	return nil
}

// ----- Job operations -----

func (s *GORMStore) GetBackupJob(ctx context.Context, id string) (*models.BackupJob, error) {
	return getByField[models.BackupJob](s.db, ctx, "id", id, models.ErrBackupJobNotFound)
}

func (s *GORMStore) ListBackupJobs(ctx context.Context, kind models.BackupJobKind, status models.BackupStatus) ([]*models.BackupJob, error) {
	q := s.db.WithContext(ctx)
	if kind != "" {
		q = q.Where("kind = ?", kind)
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var results []*models.BackupJob
	if err := q.Find(&results).Error; err != nil {
		return nil, err
	}
	return results, nil
}

func (s *GORMStore) CreateBackupJob(ctx context.Context, job *models.BackupJob) (string, error) {
	if job.ID == "" {
		job.ID = ulid.Make().String()
	}
	return createWithID(s.db, ctx, job,
		func(j *models.BackupJob, id string) { j.ID = id },
		job.ID, models.ErrDuplicateBackupJob)
}

func (s *GORMStore) UpdateBackupJob(ctx context.Context, job *models.BackupJob) error {
	result := s.db.WithContext(ctx).
		Model(&models.BackupJob{}).
		Where("id = ?", job.ID).
		Updates(map[string]any{
			"status":           job.Status,
			"started_at":       job.StartedAt,
			"finished_at":      job.FinishedAt,
			"error":            job.Error,
			"progress":         job.Progress,
			"backup_record_id": job.BackupRecordID,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return models.ErrBackupJobNotFound
	}
	return nil
}

// RecoverInterruptedJobs transitions all jobs with status=running to
// status=interrupted. Callers invoke this once on server startup so that
// jobs orphaned by a crash or forced shutdown surface a terminal state
// (SAFETY-02). Phase 5 will wire this into lifecycle.Service boot.
func (s *GORMStore) RecoverInterruptedJobs(ctx context.Context) (int, error) {
	now := time.Now()
	result := s.db.WithContext(ctx).
		Model(&models.BackupJob{}).
		Where("status = ?", models.BackupStatusRunning).
		Updates(map[string]any{
			"status":      models.BackupStatusInterrupted,
			"error":       "server restarted while job was running",
			"finished_at": now,
		})
	return int(result.RowsAffected), result.Error
}
