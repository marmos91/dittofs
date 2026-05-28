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

// UpdateSnapshotDurable flips the RemoteDurable bit on the snapshot row
// matching (shareName, id). Called by the snapshot orchestration goroutine
// (Phase 23 D-23-03) immediately before / after the ready-state flip:
// durable=true after VerifyRemoteDurability passes, durable=false on the
// --no-sync-gate path. Independent of state so the state-machine helper
// (UpdateSnapshotState) stays single-purpose.
//
// Returns models.ErrSnapshotNotFound if no row matches.
func (s *GORMStore) UpdateSnapshotDurable(ctx context.Context, shareName, id string, durable bool) error {
	db := s.db.WithContext(ctx)
	res := db.Model(&models.Snapshot{}).
		Where("share_name = ? AND id = ?", shareName, id).
		Updates(map[string]any{
			"remote_durable": durable,
			"updated_at":     time.Now(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return models.ErrSnapshotNotFound
	}
	return nil
}

// MarkSnapshotReady atomically transitions the snapshot row (shareName, id)
// from state='creating' to state='ready' AND sets remote_durable=durable in
// a single conditional UPDATE. Used by the snapshot orchestration goroutine
// (Phase 23 D-23-03) after VerifyRemoteDurability passes so the final state
// flip and the durability bit can never disagree — a half-applied update
// would leave the row indistinguishable from the intentional --no-sync-gate
// path (ready + remote_durable=false), confusing operators and Phase 24
// restore.
//
// Returns models.ErrSnapshotNotFound if no row matches (shareName, id).
// Returns models.ErrSnapshotStateConflict if the row exists but is not in
// state='creating'.
func (s *GORMStore) MarkSnapshotReady(ctx context.Context, shareName, id string, durable bool) error {
	db := s.db.WithContext(ctx)
	res := db.Model(&models.Snapshot{}).
		Where("share_name = ? AND id = ? AND state = ?", shareName, id, models.StateCreating).
		Updates(map[string]any{
			"state":          models.StateReady,
			"remote_durable": durable,
			"updated_at":     time.Now(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 1 {
		return nil
	}
	// 0 rows: either the row is absent or its prior state is not 'creating'.
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
