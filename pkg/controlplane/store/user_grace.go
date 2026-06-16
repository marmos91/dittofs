package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm/clause"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ListUserGrace returns all durable default-user grace timers for a share. Used
// to reseed the metadata service's in-memory per-user grace map at share load so
// default-user soft→grace→hard enforcement survives a restart.
func (s *GORMStore) ListUserGrace(ctx context.Context, shareName string) ([]*models.UserGrace, error) {
	var rows []*models.UserGrace
	if err := s.db.WithContext(ctx).
		Where("share_name = ?", shareName).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// SetUserGrace upserts the durable default-user grace timer for (share, uid).
// Written lazily the first time a default-user breaches their soft threshold.
func (s *GORMStore) SetUserGrace(ctx context.Context, shareName string, uid uint32, t time.Time) error {
	row := &models.UserGrace{
		ID:             uuid.New().String(),
		ShareName:      shareName,
		UID:            uid,
		GraceStartedAt: t,
	}
	// Upsert on the (share_name, uid) unique index: a concurrent enforce for the
	// same user must not error, just refresh the timestamp.
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "share_name"}, {Name: "uid"}},
		DoUpdates: clause.AssignmentColumns([]string{"grace_started_at", "updated_at"}),
	}).Create(row).Error
}

// DeleteUserGrace removes the durable default-user grace timer for (share, uid).
// Called when usage drops back under the soft threshold (reap). A missing row is
// not an error: clearing is best-effort from the enforcer hot path.
func (s *GORMStore) DeleteUserGrace(ctx context.Context, shareName string, uid uint32) error {
	return s.db.WithContext(ctx).
		Where("share_name = ? AND uid = ?", shareName, uid).
		Delete(&models.UserGrace{}).Error
}

// DeleteUserGraceForShare removes every durable default-user grace timer for a
// share. Called when the share is removed or its default-user quota is deleted,
// so stale rows cannot accumulate once the default-user fallback no longer
// applies.
func (s *GORMStore) DeleteUserGraceForShare(ctx context.Context, shareName string) error {
	return s.db.WithContext(ctx).
		Where("share_name = ?", shareName).
		Delete(&models.UserGrace{}).Error
}
