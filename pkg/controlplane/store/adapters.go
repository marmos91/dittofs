package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ============================================
// ADAPTER OPERATIONS
// ============================================

func (s *GORMStore) GetAdapter(ctx context.Context, adapterType string) (*models.AdapterConfig, error) {
	var adapter models.AdapterConfig
	if err := s.db.WithContext(ctx).Where("type = ?", adapterType).First(&adapter).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, models.ErrAdapterNotFound
		}
		return nil, err
	}
	return &adapter, nil
}

func (s *GORMStore) ListAdapters(ctx context.Context) ([]*models.AdapterConfig, error) {
	var adapters []*models.AdapterConfig
	if err := s.db.WithContext(ctx).Find(&adapters).Error; err != nil {
		return nil, err
	}
	return adapters, nil
}

func (s *GORMStore) CreateAdapter(ctx context.Context, adapter *models.AdapterConfig) (string, error) {
	if adapter.ID == "" {
		adapter.ID = uuid.New().String()
	}
	now := time.Now()
	adapter.CreatedAt = now
	adapter.UpdatedAt = now

	if err := s.db.WithContext(ctx).Create(adapter).Error; err != nil {
		if isUniqueConstraintError(err) {
			return "", models.ErrDuplicateAdapter
		}
		return "", err
	}
	return adapter.ID, nil
}

func (s *GORMStore) UpdateAdapter(ctx context.Context, adapter *models.AdapterConfig) error {
	adapter.UpdatedAt = time.Now()

	result := s.db.WithContext(ctx).
		Model(&models.AdapterConfig{}).
		Where("id = ?", adapter.ID).
		Updates(map[string]any{
			"enabled":    adapter.Enabled,
			"port":       adapter.Port,
			"config":     adapter.Config,
			"updated_at": adapter.UpdatedAt,
		})

	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return models.ErrAdapterNotFound
	}
	return nil
}

func (s *GORMStore) DeleteAdapter(ctx context.Context, adapterType string) error {
	result := s.db.WithContext(ctx).Where("type = ?", adapterType).Delete(&models.AdapterConfig{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return models.ErrAdapterNotFound
	}
	return nil
}
