package store

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ============================================
// ADAPTER SETTINGS OPERATIONS
// ============================================

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
			"blocked_operations":        settings.BlockedOperations,
			"version":                   gorm.Expr("version + 1"),
			"updated_at":                time.Now(),
		})

	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return models.ErrAdapterNotFound
	}
	return nil
}

func (s *GORMStore) ResetNFSAdapterSettings(ctx context.Context, adapterID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Verify adapter exists
		var adapter models.AdapterConfig
		if err := tx.Where("id = ?", adapterID).First(&adapter).Error; err != nil {
			return convertNotFoundError(err, models.ErrAdapterNotFound)
		}

		// Delete existing settings
		if err := tx.Where("adapter_id = ?", adapterID).Delete(&models.NFSAdapterSettings{}).Error; err != nil {
			return err
		}

		// Create new default settings
		defaults := models.NewDefaultNFSSettings(adapterID)
		return tx.Create(defaults).Error
	})
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

	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return models.ErrAdapterNotFound
	}
	return nil
}

func (s *GORMStore) ResetSMBAdapterSettings(ctx context.Context, adapterID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Verify adapter exists
		var adapter models.AdapterConfig
		if err := tx.Where("id = ?", adapterID).First(&adapter).Error; err != nil {
			return convertNotFoundError(err, models.ErrAdapterNotFound)
		}

		// Delete existing settings
		if err := tx.Where("adapter_id = ?", adapterID).Delete(&models.SMBAdapterSettings{}).Error; err != nil {
			return err
		}

		// Create new default settings
		defaults := models.NewDefaultSMBSettings(adapterID)
		return tx.Create(defaults).Error
	})
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
