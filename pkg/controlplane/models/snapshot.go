package models

import (
	"path/filepath"
	"time"
)

// Snapshot lifecycle states. The state machine is:
//
//	creating -> ready
//	creating -> failed
//	failed   -> creating   (retry; failed is not terminal)
//
// Wire strings match the JSON representation surfaced via the REST API.
const (
	StateCreating string = "creating"
	StateReady    string = "ready"
	StateFailed   string = "failed"
)

// Snapshot is the persisted record of a per-share point-in-time snapshot.
//
// The struct only declares the data shape. ID generation, CRUD, and
// AutoMigrate registration live in the snapshot store, which is shipped in a
// later plan. The unique partial index `idx_share_creating` enforces "at most
// one in-flight snapshot per share" — concurrent CreateSnapshot calls on the
// same share surface as a generic uniqueness error from the driver.
type Snapshot struct {
	ID             string    `gorm:"primaryKey;size:36" json:"id"`
	ShareName      string    `gorm:"index;not null;size:255;index:idx_share_creating,where:state='creating',unique" json:"share_name"`
	State          string    `gorm:"not null;size:20;default:'creating'" json:"state"`
	MetadataEngine string    `gorm:"not null;size:20" json:"metadata_engine"`
	ManifestCount  int64     `gorm:"not null;default:0" json:"manifest_count"`
	RemoteDurable  bool      `gorm:"not null;default:false" json:"remote_durable"`
	CreatedAt      time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt      time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

// TableName returns the table name for Snapshot.
func (Snapshot) TableName() string {
	return "snapshots"
}

// SnapshotDir returns the on-disk directory holding this snapshot's artifacts
// under the supplied share data directory: <shareDataDir>/snapshots/<id>.
func (s *Snapshot) SnapshotDir(shareDataDir string) string {
	return filepath.Join(shareDataDir, "snapshots", s.ID)
}

// ManifestPath returns the on-disk path of the hash manifest file for this
// snapshot: <SnapshotDir>/manifest.hashes.
func (s *Snapshot) ManifestPath(shareDataDir string) string {
	return filepath.Join(s.SnapshotDir(shareDataDir), "manifest.hashes")
}

// MetadataDumpPath returns the on-disk path of the engine metadata dump for
// this snapshot: <SnapshotDir>/metadata.dump.
func (s *Snapshot) MetadataDumpPath(shareDataDir string) string {
	return filepath.Join(s.SnapshotDir(shareDataDir), "metadata.dump")
}
