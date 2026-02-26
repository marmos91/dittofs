package store

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ============================================
// PAYLOAD STORE OPERATIONS
// ============================================

func (s *GORMStore) GetPayloadStore(ctx context.Context, name string) (*models.PayloadStoreConfig, error) {
	return getByField[models.PayloadStoreConfig](s.db, ctx, "name", name, models.ErrStoreNotFound)
}

func (s *GORMStore) GetPayloadStoreByID(ctx context.Context, id string) (*models.PayloadStoreConfig, error) {
	return getByField[models.PayloadStoreConfig](s.db, ctx, "id", id, models.ErrStoreNotFound)
}

func (s *GORMStore) ListPayloadStores(ctx context.Context) ([]*models.PayloadStoreConfig, error) {
	return listAll[models.PayloadStoreConfig](s.db, ctx)
}

func (s *GORMStore) CreatePayloadStore(ctx context.Context, store *models.PayloadStoreConfig) (string, error) {
	store.CreatedAt = time.Now()
	return createWithID(s.db, ctx, store, func(s *models.PayloadStoreConfig, id string) { s.ID = id }, store.ID, models.ErrDuplicateStore)
}

func (s *GORMStore) UpdatePayloadStore(ctx context.Context, store *models.PayloadStoreConfig) error {
	result := s.db.WithContext(ctx).
		Model(&models.PayloadStoreConfig{}).
		Where("id = ?", store.ID).
		Updates(map[string]any{
			"name":   store.Name,
			"type":   store.Type,
			"config": store.Config,
		})

	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return models.ErrStoreNotFound
	}
	return nil
}

func (s *GORMStore) DeletePayloadStore(ctx context.Context, name string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var store models.PayloadStoreConfig
		if err := tx.Where("name = ?", name).First(&store).Error; err != nil {
			return convertNotFoundError(err, models.ErrStoreNotFound)
		}

		// Check if any shares reference this store
		var count int64
		if err := tx.Model(&models.Share{}).Where("payload_store_id = ?", store.ID).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return models.ErrStoreInUse
		}

		// Delete store
		return tx.Delete(&store).Error
	})
}

func (s *GORMStore) GetSharesByPayloadStore(ctx context.Context, storeName string) ([]*models.Share, error) {
	store, err := s.GetPayloadStore(ctx, storeName)
	if err != nil {
		return nil, err
	}

	var shares []*models.Share
	if err := s.db.WithContext(ctx).
		Preload("MetadataStore").
		Preload("PayloadStore").
		Where("payload_store_id = ?", store.ID).
		Find(&shares).Error; err != nil {
		return nil, err
	}
	return shares, nil
}
