package models

import "time"

// Quota scope constants. Stored as a lowercase string in the scope column so
// they are stable and human-readable in the DB / REST / CLI.
const (
	// QuotaScopeUser keys a quota by a specific user (uid).
	QuotaScopeUser = "user"
	// QuotaScopeGroup keys a quota by a specific group (gid).
	QuotaScopeGroup = "group"
	// QuotaScopeDefaultUser is a fallback quota applied to any user without an
	// explicit user quota. IdentityID is unused (NULL) for this scope.
	QuotaScopeDefaultUser = "default-user"
)

// Quota is a per-identity (user/group/default-user) storage quota on a share.
// It bounds both bytes and inode (file) count, with optional soft thresholds and
// a grace period before a soft threshold is enforced as hard.
//
// Limits are stored in the control-plane DB and loaded into the metadata service
// as a hot-updatable map; both NFS and SMB honor them. A limit of 0 on any
// dimension means "no limit on that dimension".
type Quota struct {
	ID string `gorm:"primaryKey;size:36" json:"id"`

	// ShareName is the share this quota applies to.
	ShareName string `gorm:"not null;size:255;uniqueIndex:idx_quota_identity" json:"share_name"`

	// Scope is one of QuotaScopeUser / QuotaScopeGroup / QuotaScopeDefaultUser.
	Scope string `gorm:"not null;size:16;uniqueIndex:idx_quota_identity" json:"scope"`

	// IdentityID is the uid (user) or gid (group) the quota applies to. NULL for
	// the default-user scope. The unique index (share_name, scope, identity_id)
	// permits at most one quota per identity per share, and a single NULL-keyed
	// default-user row per share.
	IdentityID *uint32 `gorm:"uniqueIndex:idx_quota_identity" json:"identity_id,omitempty"`

	// LimitBytes is the hard byte ceiling (0 = unlimited).
	LimitBytes int64 `gorm:"default:0" json:"limit_bytes"`
	// SoftBytes is the soft byte threshold (0 = none).
	SoftBytes int64 `gorm:"default:0" json:"soft_bytes"`
	// LimitFiles is the hard inode (file-count) ceiling (0 = unlimited).
	LimitFiles int64 `gorm:"default:0" json:"limit_files"`
	// SoftFiles is the soft inode threshold (0 = none).
	SoftFiles int64 `gorm:"default:0" json:"soft_files"`

	// GraceSeconds is how long usage may exceed a soft threshold before it is
	// enforced as hard. 0 disables grace (soft is advisory only).
	GraceSeconds int64 `gorm:"default:0" json:"grace_seconds"`

	// GraceStartedAt records when usage first crossed a soft threshold. NULL
	// means no grace timer is running. Persisted so the timer survives restart.
	GraceStartedAt *time.Time `json:"grace_started_at,omitempty"`

	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

// TableName returns the table name for Quota.
func (Quota) TableName() string {
	return "quotas"
}
