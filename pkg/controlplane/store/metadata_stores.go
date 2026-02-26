package store

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func (s *GORMStore) GetMetadataStore(ctx context.Context, name string) (*models.MetadataStoreConfig, error) {
	return getByField[models.MetadataStoreConfig](s.db, ctx, "name", name, models.ErrStoreNotFound)
}

func (s *GORMStore) GetMetadataStoreByID(ctx context.Context, id string) (*models.MetadataStoreConfig, error) {
	return getByField[models.MetadataStoreConfig](s.db, ctx, "id", id, models.ErrStoreNotFound)
}

func (s *GORMStore) ListMetadataStores(ctx context.Context) ([]*models.MetadataStoreConfig, error) {
	return listAll[models.MetadataStoreConfig](s.db, ctx)
}

func (s *GORMStore) CreateMetadataStore(ctx context.Context, store *models.MetadataStoreConfig) (string, error) {
	store.CreatedAt = time.Now()
	return createWithID(s.db, ctx, store, func(s *models.MetadataStoreConfig, id string) { s.ID = id }, store.ID, models.ErrDuplicateStore)
}

func (s *GORMStore) UpdateMetadataStore(ctx context.Context, store *models.MetadataStoreConfig) error {
	result := s.db.WithContext(ctx).
		Model(&models.MetadataStoreConfig{}).
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

func (s *GORMStore) DeleteMetadataStore(ctx context.Context, name string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var store models.MetadataStoreConfig
		if err := tx.Where("name = ?", name).First(&store).Error; err != nil {
			return convertNotFoundError(err, models.ErrStoreNotFound)
		}

		// Check if any shares reference this store
		var count int64
		if err := tx.Model(&models.Share{}).Where("metadata_store_id = ?", store.ID).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return models.ErrStoreInUse
		}

		// Delete store
		return tx.Delete(&store).Error
	})
}

func (s *GORMStore) GetSharesByMetadataStore(ctx context.Context, storeName string) ([]*models.Share, error) {
	store, err := s.GetMetadataStore(ctx, storeName)
	if err != nil {
		return nil, err
	}

	var shares []*models.Share
	if err := s.db.WithContext(ctx).
		Preload("MetadataStore").
		Preload("PayloadStore").
		Where("metadata_store_id = ?", store.ID).
		Find(&shares).Error; err != nil {
		return nil, err
	}
	return shares, nil
}
