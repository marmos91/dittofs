package store

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func (s *GORMStore) GetAdapter(ctx context.Context, adapterType string) (*models.AdapterConfig, error) {
	return getByField[models.AdapterConfig](s.db, ctx, "type", adapterType, models.ErrAdapterNotFound)
}

func (s *GORMStore) ListAdapters(ctx context.Context) ([]*models.AdapterConfig, error) {
	return listAll[models.AdapterConfig](s.db, ctx)
}

func (s *GORMStore) CreateAdapter(ctx context.Context, adapter *models.AdapterConfig) (string, error) {
	now := time.Now()
	adapter.CreatedAt = now
	adapter.UpdatedAt = now
	return createWithID(s.db, ctx, adapter, func(a *models.AdapterConfig, id string) { a.ID = id }, adapter.ID, models.ErrDuplicateAdapter)
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
	return deleteByField[models.AdapterConfig](s.db, ctx, "type", adapterType, models.ErrAdapterNotFound)
}

// EnsureDefaultAdapters creates the default NFS and SMB adapters if they don't exist.
// Also creates default adapter settings for any adapters that lack them.
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

	// Ensure adapter settings exist for all adapters (including newly created ones)
	if err := s.EnsureAdapterSettings(ctx); err != nil {
		return created, err
	}

	return created, nil
}
