package models

import (
	"path/filepath"
	"time"
)

// Snapshot lifecycle states. State machine:
//
//	creating -> ready
//	creating -> failed
//	failed   -> creating   (retry; failed is not terminal)
const (
	StateCreating string = "creating"
	StateReady    string = "ready"
	StateFailed   string = "failed"
)

// Snapshot is the persisted record of a per-share point-in-time snapshot.
// The partial unique index idx_share_creating enforces at most one
// in-flight snapshot per share.
type Snapshot struct {
	ID             string    `gorm:"primaryKey;size:36" json:"id"`
	Name           string    `gorm:"size:255" json:"name,omitempty"`
	ShareName      string    `gorm:"index;not null;size:255;index:idx_share_creating,where:state='creating',unique" json:"share_name"`
	State          string    `gorm:"not null;size:20;default:'creating'" json:"state"`
	MetadataEngine string    `gorm:"not null;size:20" json:"metadata_engine"`
	ManifestCount  int64     `gorm:"not null;default:0" json:"manifest_count"`
	RemoteDurable  bool      `gorm:"not null;default:false" json:"remote_durable"`
	// Scheduled marks snapshots created by the background snapshot scheduler.
	// Only scheduled snapshots are eligible for automatic retention pruning;
	// manually-created snapshots are never auto-pruned.
	Scheduled bool   `gorm:"not null;default:false" json:"scheduled"`
	Error     string `gorm:"size:1024" json:"error,omitempty"`
	CreatedAt      time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt      time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

func (Snapshot) TableName() string {
	return "snapshots"
}

func (s *Snapshot) SnapshotDir(shareDataDir string) string {
	return filepath.Join(shareDataDir, "snapshots", s.ID)
}

func (s *Snapshot) ManifestPath(shareDataDir string) string {
	return filepath.Join(s.SnapshotDir(shareDataDir), "manifest.hashes")
}

func (s *Snapshot) MetadataDumpPath(shareDataDir string) string {
	return filepath.Join(s.SnapshotDir(shareDataDir), "metadata.dump")
}
