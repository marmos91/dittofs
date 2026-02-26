package store

import (
	"context"
	"time"

	"gorm.io/gorm"

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

// --- Adapter settings methods (NFS/SMB protocol-specific settings) ---

// checkUpdateResult validates a GORM update result, returning ErrAdapterNotFound
// if no rows were affected.
func checkUpdateResult(result *gorm.DB) error {
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return models.ErrAdapterNotFound
	}
	return nil
}

func (s *GORMStore) GetNFSAdapterSettings(ctx context.Context, adapterID string) (*models.NFSAdapterSettings, error) {
	var settings models.NFSAdapterSettings
	if err := s.db.WithContext(ctx).Where("adapter_id = ?", adapterID).First(&settings).Error; err != nil {
		return nil, convertNotFoundError(err, models.ErrAdapterNotFound)
	}
	return &settings, nil
}

func (s *GORMStore) UpdateNFSAdapterSettings(ctx context.Context, settings *models.NFSAdapterSettings) error {
	// Check that the settings record exists
	var existing models.NFSAdapterSettings
	if err := s.db.WithContext(ctx).Where("adapter_id = ?", settings.AdapterID).First(&existing).Error; err != nil {
		return convertNotFoundError(err, models.ErrAdapterNotFound)
	}

	// Increment version atomically and update all fields
	result := s.db.WithContext(ctx).
		Model(&models.NFSAdapterSettings{}).
		Where("id = ?", existing.ID).
		Updates(map[string]any{
			"min_version":               settings.MinVersion,
			"max_version":               settings.MaxVersion,
			"lease_time":                settings.LeaseTime,
			"grace_period":              settings.GracePeriod,
			"delegation_recall_timeout": settings.DelegationRecallTimeout,
			"callback_timeout":          settings.CallbackTimeout,
			"lease_break_timeout":       settings.LeaseBreakTimeout,
			"max_connections":           settings.MaxConnections,
			"max_clients":               settings.MaxClients,
			"max_compound_ops":          settings.MaxCompoundOps,
			"max_read_size":             settings.MaxReadSize,
			"max_write_size":            settings.MaxWriteSize,
			"preferred_transfer_size":   settings.PreferredTransferSize,
			"delegations_enabled":       settings.DelegationsEnabled,
			"max_delegations":           settings.MaxDelegations,
			"dir_deleg_batch_window_ms": settings.DirDelegBatchWindowMs,
			"blocked_operations":        settings.BlockedOperations,
			"portmapper_enabled":        settings.PortmapperEnabled,
			"portmapper_port":           settings.PortmapperPort,
			"version":                   gorm.Expr("version + 1"),
			"updated_at":                time.Now(),
		})

	return checkUpdateResult(result)
}

func (s *GORMStore) ResetNFSAdapterSettings(ctx context.Context, adapterID string) error {
	// Get existing settings to verify they exist
	var existing models.NFSAdapterSettings
	if err := s.db.WithContext(ctx).Where("adapter_id = ?", adapterID).First(&existing).Error; err != nil {
		return convertNotFoundError(err, models.ErrAdapterNotFound)
	}

	// Reset all fields to defaults while preserving identity and incrementing version.
	// Using update-in-place instead of delete+create to maintain monotonic version counter.
	defaults := models.NewDefaultNFSSettings(adapterID)
	result := s.db.WithContext(ctx).
		Model(&models.NFSAdapterSettings{}).
		Where("id = ?", existing.ID).
		Updates(map[string]any{
			"min_version":               defaults.MinVersion,
			"max_version":               defaults.MaxVersion,
			"lease_time":                defaults.LeaseTime,
			"grace_period":              defaults.GracePeriod,
			"delegation_recall_timeout": defaults.DelegationRecallTimeout,
			"callback_timeout":          defaults.CallbackTimeout,
			"lease_break_timeout":       defaults.LeaseBreakTimeout,
			"max_connections":           defaults.MaxConnections,
			"max_clients":               defaults.MaxClients,
			"max_compound_ops":          defaults.MaxCompoundOps,
			"max_read_size":             defaults.MaxReadSize,
			"max_write_size":            defaults.MaxWriteSize,
			"preferred_transfer_size":   defaults.PreferredTransferSize,
			"delegations_enabled":       defaults.DelegationsEnabled,
			"max_delegations":           defaults.MaxDelegations,
			"dir_deleg_batch_window_ms": defaults.DirDelegBatchWindowMs,
			"blocked_operations":        "",
			"portmapper_enabled":        defaults.PortmapperEnabled,
			"portmapper_port":           defaults.PortmapperPort,
			"version":                   gorm.Expr("version + 1"),
			"updated_at":                time.Now(),
		})

	return checkUpdateResult(result)
}

func (s *GORMStore) GetSMBAdapterSettings(ctx context.Context, adapterID string) (*models.SMBAdapterSettings, error) {
	var settings models.SMBAdapterSettings
	if err := s.db.WithContext(ctx).Where("adapter_id = ?", adapterID).First(&settings).Error; err != nil {
		return nil, convertNotFoundError(err, models.ErrAdapterNotFound)
	}
	return &settings, nil
}

func (s *GORMStore) UpdateSMBAdapterSettings(ctx context.Context, settings *models.SMBAdapterSettings) error {
	// Check that the settings record exists
	var existing models.SMBAdapterSettings
	if err := s.db.WithContext(ctx).Where("adapter_id = ?", settings.AdapterID).First(&existing).Error; err != nil {
		return convertNotFoundError(err, models.ErrAdapterNotFound)
	}

	// Increment version atomically and update all fields
	result := s.db.WithContext(ctx).
		Model(&models.SMBAdapterSettings{}).
		Where("id = ?", existing.ID).
		Updates(map[string]any{
			"min_dialect":          settings.MinDialect,
			"max_dialect":          settings.MaxDialect,
			"session_timeout":      settings.SessionTimeout,
			"oplock_break_timeout": settings.OplockBreakTimeout,
			"max_connections":      settings.MaxConnections,
			"max_sessions":         settings.MaxSessions,
			"enable_encryption":    settings.EnableEncryption,
			"blocked_operations":   settings.BlockedOperations,
			"version":              gorm.Expr("version + 1"),
			"updated_at":           time.Now(),
		})

	return checkUpdateResult(result)
}

func (s *GORMStore) ResetSMBAdapterSettings(ctx context.Context, adapterID string) error {
	// Get existing settings to verify they exist
	var existing models.SMBAdapterSettings
	if err := s.db.WithContext(ctx).Where("adapter_id = ?", adapterID).First(&existing).Error; err != nil {
		return convertNotFoundError(err, models.ErrAdapterNotFound)
	}

	// Reset all fields to defaults while preserving identity and incrementing version.
	// Using update-in-place instead of delete+create to maintain monotonic version counter.
	defaults := models.NewDefaultSMBSettings(adapterID)
	result := s.db.WithContext(ctx).
		Model(&models.SMBAdapterSettings{}).
		Where("id = ?", existing.ID).
		Updates(map[string]any{
			"min_dialect":          defaults.MinDialect,
			"max_dialect":          defaults.MaxDialect,
			"session_timeout":      defaults.SessionTimeout,
			"oplock_break_timeout": defaults.OplockBreakTimeout,
			"max_connections":      defaults.MaxConnections,
			"max_sessions":         defaults.MaxSessions,
			"enable_encryption":    defaults.EnableEncryption,
			"blocked_operations":   "",
			"version":              gorm.Expr("version + 1"),
			"updated_at":           time.Now(),
		})

	return checkUpdateResult(result)
}

// EnsureAdapterSettings creates default settings records for adapters that lack them.
// This is called during startup and migration to populate settings for existing adapters.
func (s *GORMStore) EnsureAdapterSettings(ctx context.Context) error {
	adapters, err := s.ListAdapters(ctx)
	if err != nil {
		return err
	}

	for _, adapter := range adapters {
		switch adapter.Type {
		case "nfs":
			var count int64
			if err := s.db.WithContext(ctx).Model(&models.NFSAdapterSettings{}).Where("adapter_id = ?", adapter.ID).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				defaults := models.NewDefaultNFSSettings(adapter.ID)
				if err := s.db.WithContext(ctx).Create(defaults).Error; err != nil {
					return err
				}
			}
		case "smb":
			var count int64
			if err := s.db.WithContext(ctx).Model(&models.SMBAdapterSettings{}).Where("adapter_id = ?", adapter.ID).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				defaults := models.NewDefaultSMBSettings(adapter.ID)
				if err := s.db.WithContext(ctx).Create(defaults).Error; err != nil {
					return err
				}
			}
		}
	}

	return nil
}
