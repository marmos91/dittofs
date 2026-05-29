package store

import (
	"context"
	"time"

	"gorm.io/gorm/clause"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// PutRestoreMarker upserts the per-share restore marker. The marker is keyed
// by share name (one in-flight restore per share); a re-write for the same
// share overwrites the existing row (e.g. a step update) rather than
// erroring. CreatedAt is preserved on update via the conflict clause.
func (s *GORMStore) PutRestoreMarker(ctx context.Context, marker *models.RestoreMarker) error {
	now := time.Now()
	if marker.CreatedAt.IsZero() {
		marker.CreatedAt = now
	}
	marker.UpdatedAt = now
	return s.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "share_name"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"target_snapshot_id", "safety_snapshot_id", "step", "updated_at",
			}),
		}).
		Create(marker).Error
}

// GetRestoreMarker returns the restore marker for shareName, or
// models.ErrRestoreMarkerNotFound if none exists.
func (s *GORMStore) GetRestoreMarker(ctx context.Context, shareName string) (*models.RestoreMarker, error) {
	var marker models.RestoreMarker
	err := s.db.WithContext(ctx).
		Where("share_name = ?", shareName).
		First(&marker).Error
	if err != nil {
		return nil, convertNotFoundError(err, models.ErrRestoreMarkerNotFound)
	}
	return &marker, nil
}

// ListRestoreMarkers returns every restore marker across all shares. Used by
// the startup recovery scan to find interrupted restores. Returns an empty
// slice (not nil) when none exist.
func (s *GORMStore) ListRestoreMarkers(ctx context.Context) ([]*models.RestoreMarker, error) {
	var markers []*models.RestoreMarker
	if err := s.db.WithContext(ctx).Find(&markers).Error; err != nil {
		return nil, err
	}
	return markers, nil
}

// DeleteRestoreMarker removes the restore marker for shareName. Deleting an
// absent marker is not an error (idempotent): a DELETE matching zero rows
// reports RowsAffected=0, not an error, so a double-clear or a clear of an
// already-recovered share is a no-op.
func (s *GORMStore) DeleteRestoreMarker(ctx context.Context, shareName string) error {
	return s.db.WithContext(ctx).
		Where("share_name = ?", shareName).
		Delete(&models.RestoreMarker{}).Error
}
