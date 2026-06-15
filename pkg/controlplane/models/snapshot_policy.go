package models

import "time"

// SnapshotPolicy is the persisted per-share schedule + retention rule that
// drives the background snapshot scheduler. At most one policy exists per share
// (enforced by the unique index on share_name). A nil LastRunAt means the
// policy has never produced a scheduled snapshot yet.
//
// Retention semantics (applied to scheduler-created snapshots only): a ready
// snapshot is pruned when it falls outside the newest KeepLast (when KeepLast>0)
// OR is older than TTL (when TTL>0). A zero bound disables that dimension.
// Manually-created snapshots (Snapshot.Scheduled=false) are never auto-pruned.
type SnapshotPolicy struct {
	ID         string        `gorm:"primaryKey;size:36" json:"id"`
	ShareName  string        `gorm:"uniqueIndex;not null;size:255" json:"share_name"`
	// No SQL default:true — GORM would coerce an explicit Go false back to the
	// default on INSERT (refs #514). Callers always set Enabled explicitly;
	// the REST/runtime layer defaults it to true when a request omits it.
	Enabled bool `gorm:"not null" json:"enabled"`
	Interval   time.Duration `gorm:"not null" json:"interval"`            // cadence between snapshots (ns)
	KeepLast   int           `gorm:"not null;default:0" json:"keep_last"` // 0 = no count bound
	TTL        time.Duration `gorm:"not null;default:0" json:"ttl"`       // 0 = no age bound (ns)
	NamePrefix string        `gorm:"size:64" json:"name_prefix,omitempty"`
	LastRunAt  *time.Time    `json:"last_run_at,omitempty"`
	CreatedAt  time.Time     `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt  time.Time     `gorm:"autoUpdateTime" json:"updated_at"`
}

func (SnapshotPolicy) TableName() string {
	return "snapshot_policies"
}
