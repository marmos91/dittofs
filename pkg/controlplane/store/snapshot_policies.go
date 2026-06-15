package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm/clause"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// UpsertSnapshotPolicy inserts or updates the policy for policy.ShareName.
// On update, the existing ID/CreatedAt/LastRunAt are preserved and only the
// schedule/retention config columns are overwritten — a config change must not
// reset the run clock. ON CONFLICT (share_name) makes this a single atomic
// statement on both SQLite and Postgres.
func (s *GORMStore) UpsertSnapshotPolicy(ctx context.Context, policy *models.SnapshotPolicy) error {
	if policy.ID == "" {
		policy.ID = uuid.New().String()
	}
	now := time.Now()
	policy.CreatedAt = now
	policy.UpdatedAt = now

	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "share_name"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"enabled", "interval", "keep_last", "ttl", "name_prefix", "updated_at",
		}),
	}).Create(policy).Error
}

func (s *GORMStore) GetSnapshotPolicy(ctx context.Context, shareName string) (*models.SnapshotPolicy, error) {
	var policy models.SnapshotPolicy
	err := s.db.WithContext(ctx).
		Where("share_name = ?", shareName).
		First(&policy).Error
	if err != nil {
		return nil, convertNotFoundError(err, models.ErrSnapshotPolicyNotFound)
	}
	return &policy, nil
}

// ListSnapshotPolicies returns every policy ordered by share_name. Returns a
// non-nil empty slice when none exist.
func (s *GORMStore) ListSnapshotPolicies(ctx context.Context) ([]*models.SnapshotPolicy, error) {
	policies := make([]*models.SnapshotPolicy, 0)
	err := s.db.WithContext(ctx).
		Order("share_name ASC").
		Find(&policies).Error
	if err != nil {
		return nil, err
	}
	return policies, nil
}

func (s *GORMStore) DeleteSnapshotPolicy(ctx context.Context, shareName string) error {
	res := s.db.WithContext(ctx).
		Where("share_name = ?", shareName).
		Delete(&models.SnapshotPolicy{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return models.ErrSnapshotPolicyNotFound
	}
	return nil
}

// TouchSnapshotPolicyRun records the last scheduler run time for shareName.
func (s *GORMStore) TouchSnapshotPolicyRun(ctx context.Context, shareName string, ranAt time.Time) error {
	res := s.db.WithContext(ctx).
		Model(&models.SnapshotPolicy{}).
		Where("share_name = ?", shareName).
		Updates(map[string]any{
			"last_run_at": ranAt,
			"updated_at":  time.Now(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return models.ErrSnapshotPolicyNotFound
	}
	return nil
}
