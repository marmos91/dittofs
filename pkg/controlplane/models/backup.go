package models

import (
	"encoding/json"
	"time"
)

// BackupStatus represents the lifecycle state of a backup record or backup/restore job.
type BackupStatus string

const (
	// BackupStatusPending indicates the backup/restore has been queued but has not started yet.
	BackupStatusPending BackupStatus = "pending"

	// BackupStatusRunning indicates the backup/restore is actively in progress.
	BackupStatusRunning BackupStatus = "running"

	// BackupStatusSucceeded indicates the backup/restore completed successfully.
	BackupStatusSucceeded BackupStatus = "succeeded"

	// BackupStatusFailed indicates the backup/restore terminated with an error.
	BackupStatusFailed BackupStatus = "failed"

	// BackupStatusInterrupted indicates the backup/restore was running when the server
	// restarted. Recovered jobs transition from running to interrupted on startup.
	BackupStatusInterrupted BackupStatus = "interrupted"
)

// BackupJobKind discriminates backup jobs from restore jobs. A single backup_jobs
// table stores both — kind is the discriminator column.
type BackupJobKind string

const (
	// BackupJobKindBackup identifies a backup job (produce a new backup record).
	BackupJobKindBackup BackupJobKind = "backup"

	// BackupJobKindRestore identifies a restore job (hydrate a metadata store from a record).
	BackupJobKindRestore BackupJobKind = "restore"
)

// BackupRepoKind discriminates backup repo destinations (local filesystem vs S3).
type BackupRepoKind string

const (
	// BackupRepoKindLocal identifies a local filesystem backup destination.
	BackupRepoKindLocal BackupRepoKind = "local"

	// BackupRepoKindS3 identifies an S3-compatible backup destination.
	BackupRepoKindS3 BackupRepoKind = "s3"
)

// BackupRepo defines a backup destination configuration scoped to a metadata store.
// A single metadata store may have multiple repos (3-2-1 strategy). Repo names are
// unique per (metadata_store_id, name) — the same name may be reused across stores.
type BackupRepo struct {
	ID              string         `gorm:"primaryKey;size:36" json:"id"`
	MetadataStoreID string         `gorm:"not null;size:36;uniqueIndex:idx_backup_repo_store_name" json:"metadata_store_id"`
	Name            string         `gorm:"not null;size:255;uniqueIndex:idx_backup_repo_store_name" json:"name"`
	Kind            BackupRepoKind `gorm:"not null;size:10;index" json:"kind"`
	Config          string         `gorm:"type:text" json:"-"` // JSON blob for destination-specific fields (path, bucket, region, prefix, ...)

	// Scheduling — nullable means no schedule set.
	Schedule *string `gorm:"size:255" json:"schedule,omitempty"` // cron expression

	// Retention policy encoded as structured columns (NOT JSON). nil = no policy.
	KeepCount   *int `json:"keep_count,omitempty"`
	KeepAgeDays *int `json:"keep_age_days,omitempty"`

	// Encryption metadata. The key itself is NEVER stored — only a reference
	// (env var name or file path) resolved at backup/restore time.
	EncryptionEnabled bool   `json:"encryption_enabled"`
	EncryptionKeyRef  string `gorm:"size:255" json:"encryption_key_ref,omitempty"`

	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at"`

	// Relationships
	MetadataStore MetadataStoreConfig `gorm:"foreignKey:MetadataStoreID" json:"metadata_store,omitzero"`

	// Parsed configuration (not stored in DB)
	ParsedConfig map[string]any `gorm:"-" json:"config,omitempty"`
}

// TableName returns the table name for BackupRepo.
func (BackupRepo) TableName() string {
	return "backup_repos"
}

// GetConfig returns the parsed destination configuration.
func (r *BackupRepo) GetConfig() (map[string]any, error) {
	if r.ParsedConfig != nil {
		return r.ParsedConfig, nil
	}
	if r.Config == "" {
		return make(map[string]any), nil
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(r.Config), &cfg); err != nil {
		return nil, err
	}
	r.ParsedConfig = cfg
	return cfg, nil
}

// SetConfig sets the destination configuration from a map.
func (r *BackupRepo) SetConfig(cfg map[string]any) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	r.Config = string(data)
	r.ParsedConfig = cfg
	return nil
}

// BackupRecord represents a single historical backup payload inside a repo.
// IDs are ULIDs (sortable, time-prefixed). Pinned records are protected from
// retention pruning (REPO-03).
type BackupRecord struct {
	ID           string       `gorm:"primaryKey;size:36" json:"id"` // ULID
	RepoID       string       `gorm:"not null;size:36;index" json:"repo_id"`
	CreatedAt    time.Time    `gorm:"autoCreateTime" json:"created_at"`
	SizeBytes    int64        `json:"size_bytes"`
	Status       BackupStatus `gorm:"not null;size:20;index" json:"status"`
	Pinned       bool         `gorm:"not null;default:false;index" json:"pinned"`
	ManifestPath string       `gorm:"size:512" json:"manifest_path"`
	SHA256       string       `gorm:"size:64" json:"sha256"`
	// StoreID is a snapshot of the source metadata store ID at backup time.
	// Used as a guard against restoring into the wrong (or a renamed) store.
	StoreID string `gorm:"size:36" json:"store_id"`
	Error   string `gorm:"type:text" json:"error,omitempty"`

	// Relationships
	Repo BackupRepo `gorm:"foreignKey:RepoID" json:"repo,omitzero"`
}

// TableName returns the table name for BackupRecord.
func (BackupRecord) TableName() string {
	return "backup_records"
}

// BackupJob tracks an in-flight backup or restore operation. A single table
// with a kind discriminator stores both (unified state machine, one polling
// endpoint, one interrupted-job recovery path).
type BackupJob struct {
	ID     string        `gorm:"primaryKey;size:36" json:"id"` // ULID
	Kind   BackupJobKind `gorm:"not null;size:10;index" json:"kind"`
	RepoID string        `gorm:"not null;size:36;index" json:"repo_id"`
	// BackupRecordID is set only when Kind == BackupJobKindRestore.
	BackupRecordID *string      `gorm:"size:36" json:"backup_record_id,omitempty"`
	Status         BackupStatus `gorm:"not null;size:20;index" json:"status"`
	StartedAt      *time.Time   `json:"started_at,omitempty"`
	FinishedAt     *time.Time   `json:"finished_at,omitempty"`
	Error          string       `gorm:"type:text" json:"error,omitempty"`
	Progress       int          `json:"progress"` // 0-100

	// Relationships
	Repo BackupRepo `gorm:"foreignKey:RepoID" json:"repo,omitzero"`
}

// TableName returns the table name for BackupJob.
func (BackupJob) TableName() string {
	return "backup_jobs"
}
