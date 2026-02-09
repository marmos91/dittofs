package store

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ============================================
// USER SHARE PERMISSION OPERATIONS
// ============================================

func (s *GORMStore) GetUserSharePermission(ctx context.Context, username, shareName string) (*models.UserSharePermission, error) {
	// Get user and share IDs
	user, err := s.GetUser(ctx, username)
	if err != nil {
		return nil, err
	}

	share, err := s.GetShare(ctx, shareName)
	if err != nil {
		return nil, err
	}

	var perm models.UserSharePermission
	if err := s.db.WithContext(ctx).
		Where("user_id = ? AND share_id = ?", user.ID, share.ID).
		First(&perm).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil // No permission is not an error
		}
		return nil, err
	}
	return &perm, nil
}

func (s *GORMStore) SetUserSharePermission(ctx context.Context, perm *models.UserSharePermission) error {
	return s.db.WithContext(ctx).Save(perm).Error
}

func (s *GORMStore) DeleteUserSharePermission(ctx context.Context, username, shareName string) error {
	// Get user and share IDs
	user, err := s.GetUser(ctx, username)
	if err != nil {
		if errors.Is(err, models.ErrUserNotFound) {
			return nil // Not an error if user doesn't exist
		}
		return err
	}

	share, err := s.GetShare(ctx, shareName)
	if err != nil {
		if errors.Is(err, models.ErrShareNotFound) {
			return nil // Not an error if share doesn't exist
		}
		return err
	}

	return s.db.WithContext(ctx).
		Where("user_id = ? AND share_id = ?", user.ID, share.ID).
		Delete(&models.UserSharePermission{}).Error
}

func (s *GORMStore) GetUserSharePermissions(ctx context.Context, username string) ([]*models.UserSharePermission, error) {
	user, err := s.GetUser(ctx, username)
	if err != nil {
		return nil, err
	}

	var perms []*models.UserSharePermission
	if err := s.db.WithContext(ctx).
		Where("user_id = ?", user.ID).
		Find(&perms).Error; err != nil {
		return nil, err
	}
	return perms, nil
}

// ============================================
// GROUP SHARE PERMISSION OPERATIONS
// ============================================

func (s *GORMStore) GetGroupSharePermission(ctx context.Context, groupName, shareName string) (*models.GroupSharePermission, error) {
	group, err := s.GetGroup(ctx, groupName)
	if err != nil {
		return nil, err
	}

	share, err := s.GetShare(ctx, shareName)
	if err != nil {
		return nil, err
	}

	var perm models.GroupSharePermission
	if err := s.db.WithContext(ctx).
		Where("group_id = ? AND share_id = ?", group.ID, share.ID).
		First(&perm).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &perm, nil
}

func (s *GORMStore) SetGroupSharePermission(ctx context.Context, perm *models.GroupSharePermission) error {
	return s.db.WithContext(ctx).Save(perm).Error
}

func (s *GORMStore) DeleteGroupSharePermission(ctx context.Context, groupName, shareName string) error {
	group, err := s.GetGroup(ctx, groupName)
	if err != nil {
		if errors.Is(err, models.ErrGroupNotFound) {
			return nil
		}
		return err
	}

	share, err := s.GetShare(ctx, shareName)
	if err != nil {
		if errors.Is(err, models.ErrShareNotFound) {
			return nil
		}
		return err
	}

	return s.db.WithContext(ctx).
		Where("group_id = ? AND share_id = ?", group.ID, share.ID).
		Delete(&models.GroupSharePermission{}).Error
}

func (s *GORMStore) GetGroupSharePermissions(ctx context.Context, groupName string) ([]*models.GroupSharePermission, error) {
	group, err := s.GetGroup(ctx, groupName)
	if err != nil {
		return nil, err
	}

	var perms []*models.GroupSharePermission
	if err := s.db.WithContext(ctx).
		Where("group_id = ?", group.ID).
		Find(&perms).Error; err != nil {
		return nil, err
	}
	return perms, nil
}

// ============================================
// PERMISSION RESOLUTION
// ============================================

func (s *GORMStore) ResolveSharePermission(ctx context.Context, user *models.User, shareName string) (models.SharePermission, error) {
	share, err := s.GetShare(ctx, shareName)
	if err != nil {
		return models.PermissionNone, err
	}

	defaultPerm := models.ParseSharePermission(share.DefaultPermission)

	if user == nil {
		return defaultPerm, nil
	}

	// Check user explicit permission first
	for _, perm := range user.SharePermissions {
		if perm.ShareID == share.ID {
			return models.ParseSharePermission(perm.Permission), nil
		}
	}

	// Check group permissions (highest wins)
	highestPerm := defaultPerm
	for _, group := range user.Groups {
		for _, perm := range share.GroupPermissions {
			if perm.GroupID == group.ID {
				p := models.ParseSharePermission(perm.Permission)
				if p.Level() > highestPerm.Level() {
					highestPerm = p
				}
			}
		}
	}

	return highestPerm, nil
}
