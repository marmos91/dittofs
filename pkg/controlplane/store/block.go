package store

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func (s *GORMStore) GetBlockStore(ctx context.Context, name string, kind models.BlockStoreKind) (*models.BlockStoreConfig, error) {
	var store models.BlockStoreConfig
	if err := s.db.WithContext(ctx).
		Where("name = ? AND kind = ?", name, kind).
		First(&store).Error; err != nil {
		return nil, convertNotFoundError(err, models.ErrStoreNotFound)
	}
	return &store, nil
}

func (s *GORMStore) GetBlockStoreByID(ctx context.Context, id string) (*models.BlockStoreConfig, error) {
	return getByField[models.BlockStoreConfig](s.db, ctx, "id", id, models.ErrStoreNotFound)
}

func (s *GORMStore) ListBlockStores(ctx context.Context, kind models.BlockStoreKind) ([]*models.BlockStoreConfig, error) {
	var results []*models.BlockStoreConfig
	if err := s.db.WithContext(ctx).
		Where("kind = ?", kind).
		Find(&results).Error; err != nil {
		return nil, err
	}
	return results, nil
}

func (s *GORMStore) CreateBlockStore(ctx context.Context, store *models.BlockStoreConfig) (string, error) {
	if store.Kind == "" {
		return "", fmt.Errorf("block store kind is required")
	}
	store.CreatedAt = time.Now()
	return createWithID(s.db, ctx, store, func(s *models.BlockStoreConfig, id string) { s.ID = id }, store.ID, models.ErrDuplicateStore)
}

func (s *GORMStore) UpdateBlockStore(ctx context.Context, store *models.BlockStoreConfig) error {
	// Kind is immutable -- only update name, type, config.
	result := s.db.WithContext(ctx).
		Model(&models.BlockStoreConfig{}).
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

func (s *GORMStore) DeleteBlockStore(ctx context.Context, name string, kind models.BlockStoreKind) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var store models.BlockStoreConfig
		if err := tx.Where("name = ? AND kind = ?", name, kind).First(&store).Error; err != nil {
			return convertNotFoundError(err, models.ErrStoreNotFound)
		}

		// Check if any shares reference this store (via local or remote block store ID)
		var count int64
		if err := tx.Model(&models.Share{}).
			Where("local_block_store_id = ? OR remote_block_store_id = ?", store.ID, store.ID).
			Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return models.ErrStoreInUse
		}

		return tx.Delete(&store).Error
	})
}

func (s *GORMStore) GetSharesByBlockStore(ctx context.Context, storeName string, kind models.BlockStoreKind) ([]*models.Share, error) {
	var store models.BlockStoreConfig
	if err := s.db.WithContext(ctx).Where("name = ? AND kind = ?", storeName, kind).First(&store).Error; err != nil {
		return nil, convertNotFoundError(err, models.ErrStoreNotFound)
	}

	var shares []*models.Share
	if err := s.db.WithContext(ctx).
		Preload("MetadataStore").
		Preload("LocalBlockStore").
		Preload("RemoteBlockStore").
		Where("local_block_store_id = ? OR remote_block_store_id = ?", store.ID, store.ID).
		Find(&shares).Error; err != nil {
		return nil, err
	}
	return shares, nil
}
