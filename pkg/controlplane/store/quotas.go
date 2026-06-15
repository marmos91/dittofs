package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ListQuotas returns all quotas for a share.
func (s *GORMStore) ListQuotas(ctx context.Context, shareName string) ([]*models.Quota, error) {
	var quotas []*models.Quota
	if err := s.db.WithContext(ctx).
		Where("share_name = ?", shareName).
		Find(&quotas).Error; err != nil {
		return nil, err
	}
	return quotas, nil
}

// ListAllQuotas returns every quota across all shares (used to seed the runtime
// at startup).
func (s *GORMStore) ListAllQuotas(ctx context.Context) ([]*models.Quota, error) {
	return listAll[models.Quota](s.db, ctx)
}

// GetQuota returns a single quota by (share, scope, identity). identityID is nil
// for the default-user scope.
func (s *GORMStore) GetQuota(ctx context.Context, shareName, scope string, identityID *uint32) (*models.Quota, error) {
	q := s.db.WithContext(ctx).Where("share_name = ? AND scope = ?", shareName, scope)
	if identityID == nil {
		q = q.Where("identity_id IS NULL")
	} else {
		q = q.Where("identity_id = ?", *identityID)
	}
	var quota models.Quota
	if err := q.First(&quota).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, models.ErrQuotaNotFound
		}
		return nil, err
	}
	return &quota, nil
}

// UpsertQuota creates or updates a quota for (share, scope, identity). The
// returned quota carries the persisted ID. A zero-limit-on-every-dimension quota
// is still stored (it may carry grace config); callers delete to remove.
func (s *GORMStore) UpsertQuota(ctx context.Context, quota *models.Quota) error {
	now := time.Now()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		existing, err := s.getQuotaTx(ctx, tx, quota.ShareName, quota.Scope, quota.IdentityID)
		switch {
		case err == nil:
			quota.ID = existing.ID
			quota.CreatedAt = existing.CreatedAt
			quota.UpdatedAt = now
			// Preserve a running grace timer unless the caller set one.
			if quota.GraceStartedAt == nil {
				quota.GraceStartedAt = existing.GraceStartedAt
			}
			return tx.Model(&models.Quota{}).Where("id = ?", existing.ID).
				Select("limit_bytes", "soft_bytes", "limit_files", "soft_files",
					"grace_seconds", "grace_started_at", "updated_at").
				Updates(quota).Error
		case errors.Is(err, models.ErrQuotaNotFound):
			if quota.ID == "" {
				quota.ID = uuid.New().String()
			}
			quota.CreatedAt = now
			quota.UpdatedAt = now
			if createErr := tx.Create(quota).Error; createErr != nil {
				// A concurrent create for the same (share, scope, identity)
				// races past the existence check and trips the unique index;
				// surface it as the domain duplicate error so the handler
				// returns 409, mirroring CreateShare.
				if isUniqueConstraintError(createErr) {
					return models.ErrDuplicateQuota
				}
				return createErr
			}
			return nil
		default:
			return err
		}
	})
}

// DeleteQuota removes a quota for (share, scope, identity).
func (s *GORMStore) DeleteQuota(ctx context.Context, shareName, scope string, identityID *uint32) error {
	q := s.db.WithContext(ctx).Where("share_name = ? AND scope = ?", shareName, scope)
	if identityID == nil {
		q = q.Where("identity_id IS NULL")
	} else {
		q = q.Where("identity_id = ?", *identityID)
	}
	res := q.Delete(&models.Quota{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return models.ErrQuotaNotFound
	}
	return nil
}

// SetQuotaGraceStartedAt persists the grace-timer transition for a quota. A nil
// t clears the timer. Missing rows are ignored (best-effort persistence from the
// enforcer hot path).
func (s *GORMStore) SetQuotaGraceStartedAt(ctx context.Context, shareName, scope string, identityID *uint32, t *time.Time) error {
	q := s.db.WithContext(ctx).Model(&models.Quota{}).
		Where("share_name = ? AND scope = ?", shareName, scope)
	if identityID == nil {
		q = q.Where("identity_id IS NULL")
	} else {
		q = q.Where("identity_id = ?", *identityID)
	}
	return q.Update("grace_started_at", t).Error
}

func (s *GORMStore) getQuotaTx(ctx context.Context, tx *gorm.DB, shareName, scope string, identityID *uint32) (*models.Quota, error) {
	q := tx.WithContext(ctx).Where("share_name = ? AND scope = ?", shareName, scope)
	if identityID == nil {
		q = q.Where("identity_id IS NULL")
	} else {
		q = q.Where("identity_id = ?", *identityID)
	}
	var quota models.Quota
	if err := q.First(&quota).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, models.ErrQuotaNotFound
		}
		return nil, err
	}
	return &quota, nil
}
