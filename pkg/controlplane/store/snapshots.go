package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// CreateSnapshot persists a new snapshot record. If snap.ID is empty a UUID is
// generated. If snap.State is empty it defaults to models.StateCreating.
// Returns models.ErrSnapshotStateConflict if another in-flight (state='creating')
// snapshot already exists for snap.ShareName — surfaced via the idx_share_creating
// partial unique index.
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

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(snap).Error; err != nil {
			if isUniqueConstraintError(err) && snap.State == models.StateCreating {
				return models.ErrSnapshotStateConflict
			}
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return snap.ID, nil
}

// GetSnapshot returns a snapshot by (shareName, snapID).
// Returns models.ErrSnapshotNotFound if no row matches.
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

// ListSnapshots returns ALL snapshots for shareName in ALL states, ordered by
// created_at DESC. Per D-14 the store does not filter by state — callers
// (HoldProvider, REST handlers) apply their own filters.
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

// DeleteSnapshot hard-deletes the snapshot row matching (shareName, snapID).
// Filesystem cleanup of the on-disk snapshot directory is the Runtime's
// responsibility (see Runtime.RemoveShare hook), not the store's.
// Returns models.ErrSnapshotNotFound if no row matches.
func (s *GORMStore) DeleteSnapshot(ctx context.Context, shareName, snapID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Where("share_name = ? AND id = ?", shareName, snapID).
			Delete(&models.Snapshot{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return models.ErrSnapshotNotFound
		}
		return nil
	})
}

// UpdateSnapshotState transitions a snapshot's state. Allowed transitions:
// creating -> ready, creating -> failed, failed -> creating (retry).
// Any other transition (including same-state) returns
// models.ErrSnapshotStateConflict.
func (s *GORMStore) UpdateSnapshotState(ctx context.Context, id, state string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var snap models.Snapshot
		if err := tx.Where("id = ?", id).First(&snap).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return models.ErrSnapshotNotFound
			}
			return err
		}
		if err := validateStateTransition(snap.State, state); err != nil {
			return err
		}
		return tx.Model(&snap).Updates(map[string]any{
			"state":      state,
			"updated_at": time.Now(),
		}).Error
	})
}

// validateStateTransition enforces the snapshot state machine. Allowed:
//
//	creating -> ready
//	creating -> failed
//	failed   -> creating   (retry)
//
// All other transitions, including same-state updates, return
// models.ErrSnapshotStateConflict.
func validateStateTransition(current, next string) error {
	switch {
	case current == models.StateCreating && next == models.StateReady:
		return nil
	case current == models.StateCreating && next == models.StateFailed:
		return nil
	case current == models.StateFailed && next == models.StateCreating:
		return nil
	default:
		return models.ErrSnapshotStateConflict
	}
}
