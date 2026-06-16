// Package dto holds the wire-level data transfer objects shared
// between the REST handlers and the typed apiclient. Both sides
// import this package; neither depends on the other.
package dto

import "time"

// Snapshot is the wire representation of a share snapshot.
type Snapshot struct {
	ID            string    `json:"id"`
	Name          string    `json:"name,omitempty"`
	Share         string    `json:"share"`
	State         string    `json:"state"`
	RemoteDurable bool      `json:"remote_durable"`
	Scheduled     bool      `json:"scheduled"`
	ManifestCount int       `json:"manifest_count,omitempty"`
	DumpBytes     int64     `json:"dump_bytes,omitempty"`
	RetryOf       string    `json:"retry_of,omitempty"`
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// CreateSnapshotRequest is the body for POST .../snapshots.
type CreateSnapshotRequest struct {
	Name     string `json:"name,omitempty"`
	NoVerify bool   `json:"no_verify,omitempty"`
	RetryOf  string `json:"retry_of,omitempty"`
}

// CreateSnapshotResponse is the 202 body returned by POST .../snapshots.
type CreateSnapshotResponse struct {
	SnapshotID string `json:"snapshot_id"`
	Share      string `json:"share"`
}

// RestoreSnapshotRequest is the body for POST .../snapshots/{id}/restore.
type RestoreSnapshotRequest struct {
	AllowNonDurable bool `json:"allow_non_durable,omitempty"`
}

// RestoreSnapshotResponse is the 200 body returned by POST .../snapshots/{id}/restore.
// SafetySnapshotID is the ID of the pre-restore safety snapshot taken
// before the destructive reset step. Empty if no safety snap was
// created (precheck or pre-verify failure).
type RestoreSnapshotResponse struct {
	SnapshotID       string `json:"snapshot_id"`
	SafetySnapshotID string `json:"safety_snapshot_id,omitempty"`
	Share            string `json:"share"`
}
