package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// GetIdentityMapping returns an identity mapping by principal.
func (s *GORMStore) GetIdentityMapping(ctx context.Context, principal string) (*models.IdentityMapping, error) {
	return getByField[models.IdentityMapping](s.db, ctx, "principal", principal, models.ErrMappingNotFound)
}

// ListIdentityMappings returns all identity mappings.
func (s *GORMStore) ListIdentityMappings(ctx context.Context) ([]*models.IdentityMapping, error) {
	return listAll[models.IdentityMapping](s.db, ctx)
}

// CreateIdentityMapping creates a new identity mapping.
func (s *GORMStore) CreateIdentityMapping(ctx context.Context, mapping *models.IdentityMapping) error {
	if mapping.ID == "" {
		mapping.ID = uuid.New().String()
	}
	result := s.db.WithContext(ctx).Create(mapping)
	if result.Error != nil {
		if isUniqueConstraintError(result.Error) {
			return models.ErrDuplicateMapping
		}
		return result.Error
	}
	return nil
}

// DeleteIdentityMapping deletes an identity mapping by principal.
func (s *GORMStore) DeleteIdentityMapping(ctx context.Context, principal string) error {
	return deleteByField[models.IdentityMapping](s.db, ctx, "principal", principal, models.ErrMappingNotFound)
}
