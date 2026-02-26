package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ============================================
// SETTINGS OPERATIONS
// ============================================

func (s *GORMStore) GetSetting(ctx context.Context, key string) (string, error) {
	var setting models.Setting
	if err := s.db.WithContext(ctx).Where("key = ?", key).First(&setting).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}
		return "", err
	}
	return setting.Value, nil
}

func (s *GORMStore) SetSetting(ctx context.Context, key, value string) error {
	setting := models.Setting{
		Key:       key,
		Value:     value,
		UpdatedAt: time.Now(),
	}
	return s.db.WithContext(ctx).Save(&setting).Error
}

func (s *GORMStore) DeleteSetting(ctx context.Context, key string) error {
	return s.db.WithContext(ctx).Where("key = ?", key).Delete(&models.Setting{}).Error
}

func (s *GORMStore) ListSettings(ctx context.Context) ([]*models.Setting, error) {
	return listAll[models.Setting](s.db, ctx)
}
