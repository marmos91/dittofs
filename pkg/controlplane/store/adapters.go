package store

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ============================================
// ADAPTER OPERATIONS
// ============================================

func (s *GORMStore) GetAdapter(ctx context.Context, adapterType string) (*models.AdapterConfig, error) {
	var adapter models.AdapterConfig
	if err := s.db.WithContext(ctx).Where("type = ?", adapterType).First(&adapter).Error; err != nil {
		return nil, convertNotFoundError(err, models.ErrAdapterNotFound)
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

// EnsureDefaultAdapters creates the default NFS and SMB adapters if they don't exist.
// Returns true if any adapters were created.
func (s *GORMStore) EnsureDefaultAdapters(ctx context.Context) (bool, error) {
	created := false

	// Default adapter configurations
	defaults := []struct {
		adapterType string
		port        int
	}{
		{"nfs", 12049},
		{"smb", 1445},
	}

	for _, d := range defaults {
		_, err := s.GetAdapter(ctx, d.adapterType)
		if err == nil {
			continue // Already exists
		}
		if err != models.ErrAdapterNotFound {
			return created, err // Unexpected error
		}

		// Create the adapter
		adapter := &models.AdapterConfig{
			Type:    d.adapterType,
			Port:    d.port,
			Enabled: true,
		}
		if _, err := s.CreateAdapter(ctx, adapter); err != nil {
			return created, err
		}
		created = true
	}

	return created, nil
}
