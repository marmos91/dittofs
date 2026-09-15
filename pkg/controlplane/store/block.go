package store

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func (s *GORMStore) GetBlockStore(ctx context.Context, name string) (*models.BlockStoreConfig, error) {
	// Resolve by name then by ID (docker-style name-or-ID addressing).
	return getByNameOrID[models.BlockStoreConfig](s.db, ctx, "name", name, models.ErrStoreNotFound)
}

func (s *GORMStore) GetBlockStoreByID(ctx context.Context, id string) (*models.BlockStoreConfig, error) {
	return getByField[models.BlockStoreConfig](s.db, ctx, "id", id, models.ErrStoreNotFound)
}

func (s *GORMStore) ListBlockStores(ctx context.Context) ([]*models.BlockStoreConfig, error) {
	var results []*models.BlockStoreConfig
	if err := s.db.WithContext(ctx).
		Find(&results).Error; err != nil {
		return nil, err
	}
	return results, nil
}

func (s *GORMStore) CreateBlockStore(ctx context.Context, store *models.BlockStoreConfig) (string, error) {
	store.CreatedAt = time.Now()
	return createWithID(s.db, ctx, store, func(s *models.BlockStoreConfig, id string) { s.ID = id }, store.ID, models.ErrDuplicateStore)
}

func (s *GORMStore) UpdateBlockStore(ctx context.Context, store *models.BlockStoreConfig) error {
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

func (s *GORMStore) DeleteBlockStore(ctx context.Context, name string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		store, err := getByNameOrID[models.BlockStoreConfig](tx, ctx, "name", name, models.ErrStoreNotFound)
		if err != nil {
			return err
		}

		// Check if any shares reference this store. A share's binding normally
		// holds the store's UUID, but the older update path persisted the name
		// instead, and both still resolve at load time. Counting only the UUID
		// would let the store be deleted out from under a name-bound share,
		// leaving a binding that resolves to nothing at the next restart.
		var count int64
		if err := tx.Model(&models.Share{}).
			Where("block_store_id IN ?", []string{store.ID, store.Name}).
			Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return models.ErrStoreInUse
		}

		return tx.Delete(store).Error
	})
}

func (s *GORMStore) GetSharesByBlockStore(ctx context.Context, storeName string) ([]*models.Share, error) {
	var store models.BlockStoreConfig
	if err := s.db.WithContext(ctx).Where("name = ?", storeName).First(&store).Error; err != nil {
		return nil, convertNotFoundError(err, models.ErrStoreNotFound)
	}

	var shares []*models.Share
	if err := s.db.WithContext(ctx).
		Preload("MetadataStore").
		Preload("BlockStore").
		Where("block_store_id = ?", store.ID).
		Find(&shares).Error; err != nil {
		return nil, err
	}
	return shares, nil
}

// RenameBlockStore gives a block store a new name and returns the updated
// configuration.
//
// A name identifies a block store on its own, so a rename onto a name already
// in use is refused with models.ErrDuplicateStore rather than creating the
// collision that startup would then have to reject.
//
// The rename and the share repointing share one transaction because they are
// one change: a share's binding normally holds the store's UUID, but the older
// update path persisted the name instead, and such a share resolves nothing
// once the name moves. Repointing lands on the UUID, which no later rename can
// invalidate. Splitting the two would leave a window where the store has its
// new name and those shares point at a store that no longer answers to it.
func (s *GORMStore) RenameBlockStore(ctx context.Context, name, newName string) (*models.BlockStoreConfig, error) {
	var renamed models.BlockStoreConfig
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		store, err := getByNameOrID[models.BlockStoreConfig](tx, ctx, "name", name, models.ErrStoreNotFound)
		if err != nil {
			return err
		}
		if store.Name == newName {
			renamed = *store
			return nil
		}

		var collisions int64
		if err := tx.Model(&models.BlockStoreConfig{}).
			Where("name = ? AND id != ?", newName, store.ID).
			Count(&collisions).Error; err != nil {
			return err
		}
		if collisions > 0 {
			return models.ErrDuplicateStore
		}

		if err := tx.Model(&models.BlockStoreConfig{}).
			Where("id = ?", store.ID).
			Update("name", newName).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.Share{}).
			Where("block_store_id = ?", store.Name).
			Update("block_store_id", store.ID).Error; err != nil {
			return err
		}

		store.Name = newName
		renamed = *store
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &renamed, nil
}
