package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// GetIdentityMapping returns an identity mapping by principal.
func (s *GORMStore) GetIdentityMapping(ctx context.Context, principal string) (*models.IdentityMapping, error) {
	var mapping models.IdentityMapping
	result := s.db.WithContext(ctx).Where("principal = ?", principal).First(&mapping)
	if result.Error != nil {
		return nil, convertNotFoundError(result.Error, models.ErrMappingNotFound)
	}
	return &mapping, nil
}

// ListIdentityMappings returns all identity mappings.
func (s *GORMStore) ListIdentityMappings(ctx context.Context) ([]*models.IdentityMapping, error) {
	var mappings []*models.IdentityMapping
	result := s.db.WithContext(ctx).Find(&mappings)
	if result.Error != nil {
		return nil, result.Error
	}
	return mappings, nil
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
	result := s.db.WithContext(ctx).Where("principal = ?", principal).Delete(&models.IdentityMapping{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return models.ErrMappingNotFound
	}
	return nil
}
