package models

import "time"

// UserGrace is the durable per-real-user grace timer for a default-user quota
// fallback. A default-user quota is a single shared template row (see Quota with
// QuotaScopeDefaultUser) applied to every uid without an explicit quota, so its
// grace state is inherently per-user: each user crosses the soft threshold at a
// different time. That per-user state cannot live on the single shared Quota
// row, so it is recorded here, keyed by (share_name, uid).
//
// Rows are written lazily the first time a default-user breaches their soft
// threshold and reaped when usage drops back under soft, so growth is bounded by
// the number of default-users currently in a grace window. Persisting these
// makes default-user soft→grace→hard enforcement survive a server restart (the
// multi-tenant common case where a default cap applies to everyone); without it
// a restart hands every over-soft default user a fresh grace window.
//
// Explicit user/group quotas keep their own grace timer on the Quota row and do
// not use this table.
type UserGrace struct {
	ID string `gorm:"primaryKey;size:36" json:"id"`

	// ShareName is the share the grace timer belongs to.
	ShareName string `gorm:"not null;size:255;uniqueIndex:idx_user_grace_identity" json:"share_name"`

	// UID is the real user id whose default-user grace window this records.
	UID uint32 `gorm:"not null;uniqueIndex:idx_user_grace_identity" json:"uid"`

	// GraceStartedAt records when this user first crossed the default-user soft
	// threshold. Always set for a stored row (the row is deleted to clear).
	GraceStartedAt time.Time `gorm:"not null" json:"grace_started_at"`

	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

// TableName returns the table name for UserGrace.
func (UserGrace) TableName() string {
	return "user_grace"
}
