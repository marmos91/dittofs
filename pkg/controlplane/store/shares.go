package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ============================================
// SHARE OPERATIONS
// ============================================

func (s *GORMStore) GetShare(ctx context.Context, name string) (*models.Share, error) {
	var share models.Share
	err := s.db.WithContext(ctx).
		Preload("MetadataStore").
		Preload("PayloadStore").
		Preload("AccessRules").
		Preload("UserPermissions").
		Preload("GroupPermissions").
		Where("name = ?", name).
		First(&share).Error
	if err != nil {
		return nil, convertNotFoundError(err, models.ErrShareNotFound)
	}
	return &share, nil
}

func (s *GORMStore) GetShareByID(ctx context.Context, id string) (*models.Share, error) {
	var share models.Share
	err := s.db.WithContext(ctx).
		Preload("MetadataStore").
		Preload("PayloadStore").
		Preload("AccessRules").
		Preload("UserPermissions").
		Preload("GroupPermissions").
		Where("id = ?", id).
		First(&share).Error
	if err != nil {
		return nil, convertNotFoundError(err, models.ErrShareNotFound)
	}
	return &share, nil
}

func (s *GORMStore) ListShares(ctx context.Context) ([]*models.Share, error) {
	var shares []*models.Share
	if err := s.db.WithContext(ctx).
		Preload("MetadataStore").
		Preload("PayloadStore").
		Find(&shares).Error; err != nil {
		return nil, err
	}
	return shares, nil
}

func (s *GORMStore) CreateShare(ctx context.Context, share *models.Share) (string, error) {
	if share.ID == "" {
		share.ID = uuid.New().String()
	}
	now := time.Now()
	share.CreatedAt = now
	share.UpdatedAt = now

	if err := s.db.WithContext(ctx).Create(share).Error; err != nil {
		if isUniqueConstraintError(err) {
			return "", models.ErrDuplicateShare
		}
		return "", err
	}
	return share.ID, nil
}

func (s *GORMStore) UpdateShare(ctx context.Context, share *models.Share) error {
	share.UpdatedAt = time.Now()

	result := s.db.WithContext(ctx).
		Model(&models.Share{}).
		Where("id = ?", share.ID).
		Updates(map[string]any{
			"name":               share.Name,
			"metadata_store_id":  share.MetadataStoreID,
			"payload_store_id":   share.PayloadStoreID,
			"read_only":          share.ReadOnly,
			"default_permission": share.DefaultPermission,
			"config":             share.Config,
			"guest_enabled":      share.GuestEnabled,
			"guest_uid":          share.GuestUID,
			"guest_gid":          share.GuestGID,
			"updated_at":         share.UpdatedAt,
		})

	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return models.ErrShareNotFound
	}
	return nil
}

func (s *GORMStore) DeleteShare(ctx context.Context, name string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var share models.Share
		if err := tx.Where("name = ?", name).First(&share).Error; err != nil {
			return convertNotFoundError(err, models.ErrShareNotFound)
		}

		// Delete access rules
		if err := tx.Where("share_id = ?", share.ID).Delete(&models.ShareAccessRule{}).Error; err != nil {
			return err
		}

		// Delete user permissions
		if err := tx.Where("share_id = ?", share.ID).Delete(&models.UserSharePermission{}).Error; err != nil {
			return err
		}

		// Delete group permissions
		if err := tx.Where("share_id = ?", share.ID).Delete(&models.GroupSharePermission{}).Error; err != nil {
			return err
		}

		// Delete share
		return tx.Delete(&share).Error
	})
}

func (s *GORMStore) GetUserAccessibleShares(ctx context.Context, username string) ([]*models.Share, error) {
	user, err := s.GetUser(ctx, username)
	if err != nil {
		return nil, err
	}

	// Get share IDs from user permissions
	shareIDs := make(map[string]bool)
	for _, perm := range user.SharePermissions {
		if models.SharePermission(perm.Permission).CanRead() {
			shareIDs[perm.ShareID] = true
		}
	}

	// Get share IDs from group permissions
	for _, group := range user.Groups {
		for _, perm := range group.SharePermissions {
			if models.SharePermission(perm.Permission).CanRead() {
				shareIDs[perm.ShareID] = true
			}
		}
	}

	// Also include shares where default permission allows access
	allShares, err := s.ListShares(ctx)
	if err != nil {
		return nil, err
	}

	var accessibleShares []*models.Share
	for _, share := range allShares {
		// Already have explicit permission
		if shareIDs[share.ID] {
			accessibleShares = append(accessibleShares, share)
			continue
		}
		// Check default permission
		defaultPerm := models.ParseSharePermission(share.DefaultPermission)
		if defaultPerm.CanRead() {
			accessibleShares = append(accessibleShares, share)
		}
	}

	return accessibleShares, nil
}

// ============================================
// SHARE ACCESS RULE OPERATIONS
// ============================================

func (s *GORMStore) GetShareAccessRules(ctx context.Context, shareName string) ([]*models.ShareAccessRule, error) {
	share, err := s.GetShare(ctx, shareName)
	if err != nil {
		return nil, err
	}

	rules := make([]*models.ShareAccessRule, len(share.AccessRules))
	for i := range share.AccessRules {
		rules[i] = &share.AccessRules[i]
	}
	return rules, nil
}

func (s *GORMStore) SetShareAccessRules(ctx context.Context, shareName string, rules []*models.ShareAccessRule) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var share models.Share
		if err := tx.Where("name = ?", shareName).First(&share).Error; err != nil {
			return convertNotFoundError(err, models.ErrShareNotFound)
		}

		// Delete existing rules
		if err := tx.Where("share_id = ?", share.ID).Delete(&models.ShareAccessRule{}).Error; err != nil {
			return err
		}

		// Create new rules
		for _, rule := range rules {
			if rule.ID == "" {
				rule.ID = uuid.New().String()
			}
			rule.ShareID = share.ID
			if err := tx.Create(rule).Error; err != nil {
				return err
			}
		}

		return nil
	})
}

func (s *GORMStore) AddShareAccessRule(ctx context.Context, shareName string, rule *models.ShareAccessRule) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var share models.Share
		if err := tx.Where("name = ?", shareName).First(&share).Error; err != nil {
			return convertNotFoundError(err, models.ErrShareNotFound)
		}

		if rule.ID == "" {
			rule.ID = uuid.New().String()
		}
		rule.ShareID = share.ID

		return tx.Create(rule).Error
	})
}

func (s *GORMStore) RemoveShareAccessRule(ctx context.Context, shareName, ruleID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var share models.Share
		if err := tx.Where("name = ?", shareName).First(&share).Error; err != nil {
			return convertNotFoundError(err, models.ErrShareNotFound)
		}

		return tx.Where("id = ? AND share_id = ?", ruleID, share.ID).Delete(&models.ShareAccessRule{}).Error
	})
}

// ============================================
// GUEST ACCESS OPERATIONS (per-share)
// ============================================

func (s *GORMStore) GetGuestUser(ctx context.Context, shareName string) (*models.User, error) {
	share, err := s.GetShare(ctx, shareName)
	if err != nil {
		return nil, err
	}

	if !share.GuestEnabled {
		return nil, models.ErrGuestDisabled
	}

	// Create a pseudo-user for guest
	return &models.User{
		Username:    "guest",
		Enabled:     true,
		Role:        string(models.RoleUser),
		UID:         share.GuestUID,
		GID:         share.GuestGID,
		DisplayName: "Guest",
	}, nil
}

func (s *GORMStore) IsGuestEnabled(ctx context.Context, shareName string) bool {
	share, err := s.GetShare(ctx, shareName)
	if err != nil {
		return false
	}
	return share.GuestEnabled
}
