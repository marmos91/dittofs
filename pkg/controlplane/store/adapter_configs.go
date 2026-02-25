package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ============================================
// SHARE ADAPTER CONFIG OPERATIONS
// ============================================

func (s *GORMStore) GetShareAdapterConfig(ctx context.Context, shareID, adapterType string) (*models.ShareAdapterConfig, error) {
	var config models.ShareAdapterConfig
	err := s.db.WithContext(ctx).
		Where("share_id = ? AND adapter_type = ?", shareID, adapterType).
		First(&config).Error
	if err != nil {
		return nil, convertNotFoundError(err, nil)
	}
	return &config, nil
}

func (s *GORMStore) SetShareAdapterConfig(ctx context.Context, config *models.ShareAdapterConfig) error {
	if config.ID == "" {
		config.ID = uuid.New().String()
	}
	now := time.Now()
	config.UpdatedAt = now

	// Try to find existing record
	var existing models.ShareAdapterConfig
	err := s.db.WithContext(ctx).
		Where("share_id = ? AND adapter_type = ?", config.ShareID, config.AdapterType).
		First(&existing).Error
	if err == nil {
		// Update existing record
		return s.db.WithContext(ctx).
			Model(&existing).
			Updates(map[string]any{
				"config":     config.Config,
				"updated_at": now,
			}).Error
	}

	// Create new record
	config.CreatedAt = now
	return s.db.WithContext(ctx).Create(config).Error
}

func (s *GORMStore) DeleteShareAdapterConfig(ctx context.Context, shareID, adapterType string) error {
	return s.db.WithContext(ctx).
		Where("share_id = ? AND adapter_type = ?", shareID, adapterType).
		Delete(&models.ShareAdapterConfig{}).Error
}

func (s *GORMStore) ListShareAdapterConfigs(ctx context.Context, shareID string) ([]models.ShareAdapterConfig, error) {
	var configs []models.ShareAdapterConfig
	if err := s.db.WithContext(ctx).
		Where("share_id = ?", shareID).
		Find(&configs).Error; err != nil {
		return nil, err
	}
	return configs, nil
}
