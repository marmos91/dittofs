package store

import (
	"context"
	"time"

	"github.com/google/uuid"

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
//
// The check-then-update is atomic at the DB level: a single conditional
// UPDATE matches both the (shareName, id) tuple and the expected prior
// state. RowsAffected disambiguates "no such row" from "state already
// moved" without a separate SELECT, which would race under READ COMMITTED.
func (s *GORMStore) UpdateSnapshotState(ctx context.Context, shareName, id, state string) error {
	priors := allowedPriorStates(state)
	if len(priors) == 0 {
		return models.ErrSnapshotStateConflict
	}
	db := s.db.WithContext(ctx)
	res := db.Model(&models.Snapshot{}).
		Where("share_name = ? AND id = ? AND state IN ?", shareName, id, priors).
		Updates(map[string]any{
			"state":      state,
			"updated_at": time.Now(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 1 {
		return nil
	}
	// 0 rows: either row absent or prior state not in the allowed set.
	var count int64
	if err := db.Model(&models.Snapshot{}).
		Where("share_name = ? AND id = ?", shareName, id).
		Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return models.ErrSnapshotNotFound
	}
	return models.ErrSnapshotStateConflict
}

// allowedPriorStates returns the set of states from which `next` is reachable.
// Transitions: creating -> ready, creating -> failed, failed -> creating.
func allowedPriorStates(next string) []string {
	switch next {
	case models.StateReady, models.StateFailed:
		return []string{models.StateCreating}
	case models.StateCreating:
		return []string{models.StateFailed}
	}
	return nil
}
