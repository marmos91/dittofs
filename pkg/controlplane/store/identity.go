package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// GetIdentityMapping returns an identity mapping by provider and principal.
func (s *GORMStore) GetIdentityMapping(ctx context.Context, provider, principal string) (*models.IdentityMapping, error) {
	var mapping models.IdentityMapping
	err := s.db.WithContext(ctx).
		Where("provider_name = ? AND principal = ?", provider, principal).
		First(&mapping).Error
	if err != nil {
		return nil, convertNotFoundError(err, models.ErrMappingNotFound)
	}
	return &mapping, nil
}

// ListIdentityMappings returns identity mappings, optionally filtered by provider.
func (s *GORMStore) ListIdentityMappings(ctx context.Context, provider string) ([]*models.IdentityMapping, error) {
	var mappings []*models.IdentityMapping
	query := s.db.WithContext(ctx)
	if provider != "" {
		query = query.Where("provider_name = ?", provider)
	}
	if err := query.Find(&mappings).Error; err != nil {
		return nil, err
	}
	return mappings, nil
}

// CreateIdentityMapping creates a new identity mapping.
func (s *GORMStore) CreateIdentityMapping(ctx context.Context, mapping *models.IdentityMapping) error {
	if mapping.ID == "" {
		mapping.ID = uuid.New().String()
	}
	if mapping.ProviderName == "" {
		mapping.ProviderName = "kerberos"
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

// DeleteIdentityMapping deletes an identity mapping by provider and principal.
func (s *GORMStore) DeleteIdentityMapping(ctx context.Context, provider, principal string) error {
	result := s.db.WithContext(ctx).
		Where("provider_name = ? AND principal = ?", provider, principal).
		Delete(&models.IdentityMapping{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return models.ErrMappingNotFound
	}
	return nil
}

// ListIdentityMappingsForUser returns all identity mappings for a DittoFS user.
func (s *GORMStore) ListIdentityMappingsForUser(ctx context.Context, username string) ([]*models.IdentityMapping, error) {
	var mappings []*models.IdentityMapping
	if err := s.db.WithContext(ctx).Where("username = ?", username).Find(&mappings).Error; err != nil {
		return nil, err
	}
	return mappings, nil
}
