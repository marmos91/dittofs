package store

import (
	"context"

	"gorm.io/gorm/clause"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// GetIdentityProviderConfig returns the configuration row for a provider type.
// Returns models.ErrIdentityProviderConfigNotFound when no row exists.
func (s *GORMStore) GetIdentityProviderConfig(ctx context.Context, providerType string) (*models.IdentityProviderConfig, error) {
	var cfg models.IdentityProviderConfig
	err := s.db.WithContext(ctx).Where("type = ?", providerType).First(&cfg).Error
	if err != nil {
		return nil, convertNotFoundError(err, models.ErrIdentityProviderConfigNotFound)
	}
	return &cfg, nil
}

// ListIdentityProviderConfigs returns all configured provider rows.
func (s *GORMStore) ListIdentityProviderConfigs(ctx context.Context) ([]*models.IdentityProviderConfig, error) {
	return listAll[models.IdentityProviderConfig](s.db, ctx)
}

// PutIdentityProviderConfig creates or replaces the configuration row for
// cfg.Type. The upsert is keyed on the type primary key and overwrites the
// enabled flag and config blob.
func (s *GORMStore) PutIdentityProviderConfig(ctx context.Context, cfg *models.IdentityProviderConfig) error {
	return s.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "type"}},
			DoUpdates: clause.AssignmentColumns([]string{"enabled", "config", "updated_at"}),
		}).
		Create(cfg).Error
}

// DeleteIdentityProviderConfig removes the configuration row for a provider
// type. Returns models.ErrIdentityProviderConfigNotFound when no row exists.
func (s *GORMStore) DeleteIdentityProviderConfig(ctx context.Context, providerType string) error {
	return deleteByField[models.IdentityProviderConfig](s.db, ctx, "type", providerType, models.ErrIdentityProviderConfigNotFound)
}

// Compile-time assertion that GORMStore satisfies IdentityProviderConfigStore.
var _ IdentityProviderConfigStore = (*GORMStore)(nil)
