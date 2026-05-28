package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// CreateSnapshot persists a new snapshot record. If snap.ID is empty a UUID
// is generated; if snap.State is empty it defaults to models.StateCreating.
// Returns models.ErrSnapshotStateConflict when another in-flight snapshot
// already exists for snap.ShareName (surfaced via the idx_share_creating
// partial unique index).
func (s *GORMStore) CreateSnapshot(ctx context.Context, snap *models.Snapshot) (string, error) {
	if snap.ID == "" {
		snap.ID = uuid.New().String()
	}
	if snap.State == "" {
		snap.State = models.StateCreating
	}
	now := time.Now()
	snap.CreatedAt = now
	snap.UpdatedAt = now

	if err := s.db.WithContext(ctx).Create(snap).Error; err != nil {
		if isUniqueConstraintError(err) && snap.State == models.StateCreating {
			return "", models.ErrSnapshotStateConflict
		}
		return "", err
	}
	return snap.ID, nil
}

func (s *GORMStore) GetSnapshot(ctx context.Context, shareName, snapID string) (*models.Snapshot, error) {
	var snap models.Snapshot
	err := s.db.WithContext(ctx).
		Where("share_name = ? AND id = ?", shareName, snapID).
		First(&snap).Error
	if err != nil {
		return nil, convertNotFoundError(err, models.ErrSnapshotNotFound)
	}
	return &snap, nil
}

// ListSnapshots returns ALL snapshots for shareName in ALL states, ordered
// by created_at DESC. Callers filter by state as needed.
func (s *GORMStore) ListSnapshots(ctx context.Context, shareName string) ([]*models.Snapshot, error) {
	var snaps []*models.Snapshot
	err := s.db.WithContext(ctx).
		Where("share_name = ?", shareName).
		Order("created_at DESC").
		Find(&snaps).Error
	if err != nil {
		return nil, err
	}
	return snaps, nil
}

// DeleteSnapshot hard-deletes the row matching (shareName, snapID).
// Filesystem cleanup of the on-disk snapshot directory is the Runtime's
// responsibility.
func (s *GORMStore) DeleteSnapshot(ctx context.Context, shareName, snapID string) error {
	result := s.db.WithContext(ctx).
		Where("share_name = ? AND id = ?", shareName, snapID).
		Delete(&models.Snapshot{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return models.ErrSnapshotNotFound
	}
	return nil
}

// UpdateSnapshotState transitions a snapshot's state. Allowed transitions:
// creating -> ready, creating -> failed, failed -> creating. Any other
// transition (including same-state) returns models.ErrSnapshotStateConflict.
// shareName scopes the update so an id collision across shares cannot
// rewrite the wrong row.
func (s *GORMStore) UpdateSnapshotState(ctx context.Context, shareName, id, state string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var snap models.Snapshot
		if err := tx.Where("share_name = ? AND id = ?", shareName, id).First(&snap).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return models.ErrSnapshotNotFound
			}
			return err
		}
		if !isAllowedStateTransition(snap.State, state) {
			return models.ErrSnapshotStateConflict
		}
		return tx.Model(&snap).Updates(map[string]any{
			"state":      state,
			"updated_at": time.Now(),
		}).Error
	})
}

func isAllowedStateTransition(current, next string) bool {
	switch current {
	case models.StateCreating:
		return next == models.StateReady || next == models.StateFailed
	case models.StateFailed:
		return next == models.StateCreating
	}
	return false
}
