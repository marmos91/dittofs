package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ============================================
// GROUP OPERATIONS
// ============================================

func (s *GORMStore) GetGroup(ctx context.Context, name string) (*models.Group, error) {
	var group models.Group
	err := s.db.WithContext(ctx).
		Preload("SharePermissions").
		Where("name = ?", name).
		First(&group).Error
	if err != nil {
		return nil, convertNotFoundError(err, models.ErrGroupNotFound)
	}
	return &group, nil
}

func (s *GORMStore) GetGroupByID(ctx context.Context, id string) (*models.Group, error) {
	var group models.Group
	err := s.db.WithContext(ctx).
		Preload("SharePermissions").
		Where("id = ?", id).
		First(&group).Error
	if err != nil {
		return nil, convertNotFoundError(err, models.ErrGroupNotFound)
	}
	return &group, nil
}

func (s *GORMStore) ListGroups(ctx context.Context) ([]*models.Group, error) {
	var groups []*models.Group
	if err := s.db.WithContext(ctx).
		Preload("SharePermissions").
		Find(&groups).Error; err != nil {
		return nil, err
	}
	return groups, nil
}

func (s *GORMStore) CreateGroup(ctx context.Context, group *models.Group) (string, error) {
	if group.ID == "" {
		group.ID = uuid.New().String()
	}
	group.CreatedAt = time.Now()

	if err := s.db.WithContext(ctx).Create(group).Error; err != nil {
		if isUniqueConstraintError(err) {
			return "", models.ErrDuplicateGroup
		}
		return "", err
	}
	return group.ID, nil
}

func (s *GORMStore) UpdateGroup(ctx context.Context, group *models.Group) error {
	// Check if group exists first
	var existing models.Group
	if err := s.db.WithContext(ctx).Where("id = ?", group.ID).First(&existing).Error; err != nil {
		return convertNotFoundError(err, models.ErrGroupNotFound)
	}

	// Update specific fields using Select to handle pointers properly
	return s.db.WithContext(ctx).
		Model(&existing).
		Select("Name", "GID", "Description").
		Updates(group).Error
}

func (s *GORMStore) DeleteGroup(ctx context.Context, name string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var group models.Group
		if err := tx.Where("name = ?", name).First(&group).Error; err != nil {
			return convertNotFoundError(err, models.ErrGroupNotFound)
		}

		// Delete share permissions
		if err := tx.Where("group_id = ?", group.ID).Delete(&models.GroupSharePermission{}).Error; err != nil {
			return err
		}

		// Remove users from group (GORM handles the join table)
		if err := tx.Model(&group).Association("Users").Clear(); err != nil {
			return err
		}

		// Delete group
		if err := tx.Delete(&group).Error; err != nil {
			return err
		}

		return nil
	})
}

func (s *GORMStore) GetUserGroups(ctx context.Context, username string) ([]*models.Group, error) {
	user, err := s.GetUser(ctx, username)
	if err != nil {
		return nil, err
	}

	groups := make([]*models.Group, len(user.Groups))
	for i := range user.Groups {
		groups[i] = &user.Groups[i]
	}
	return groups, nil
}

func (s *GORMStore) AddUserToGroup(ctx context.Context, username, groupName string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user models.User
		if err := tx.Where("username = ?", username).First(&user).Error; err != nil {
			return convertNotFoundError(err, models.ErrUserNotFound)
		}

		var group models.Group
		if err := tx.Where("name = ?", groupName).First(&group).Error; err != nil {
			return convertNotFoundError(err, models.ErrGroupNotFound)
		}

		return tx.Model(&user).Association("Groups").Append(&group)
	})
}

func (s *GORMStore) RemoveUserFromGroup(ctx context.Context, username, groupName string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user models.User
		if err := tx.Where("username = ?", username).First(&user).Error; err != nil {
			return convertNotFoundError(err, models.ErrUserNotFound)
		}

		var group models.Group
		if err := tx.Where("name = ?", groupName).First(&group).Error; err != nil {
			// Group not found is not an error for remove operation
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}

		return tx.Model(&user).Association("Groups").Delete(&group)
	})
}

func (s *GORMStore) GetGroupMembers(ctx context.Context, groupName string) ([]*models.User, error) {
	var group models.Group
	if err := s.db.WithContext(ctx).Where("name = ?", groupName).First(&group).Error; err != nil {
		return nil, convertNotFoundError(err, models.ErrGroupNotFound)
	}

	// Get all users who belong to this group
	var users []*models.User
	if err := s.db.WithContext(ctx).
		Joins("JOIN user_groups ON user_groups.user_id = users.id").
		Joins("JOIN groups ON groups.id = user_groups.group_id").
		Where("groups.name = ?", groupName).
		Find(&users).Error; err != nil {
		return nil, err
	}

	return users, nil
}

// EnsureDefaultGroups creates the default groups (admins, users) if they don't exist.
// Also adds the admin user to the admins group if both exist.
// Returns true if any groups were created.
func (s *GORMStore) EnsureDefaultGroups(ctx context.Context) (bool, error) {
	created := false

	// Helper to create uint32 pointer
	uint32Ptr := func(v uint32) *uint32 { return &v }

	defaults := []struct {
		name        string
		gid         *uint32
		description string
	}{
		{"admins", uint32Ptr(1000), "System administrators"},
		{"users", uint32Ptr(1001), "Regular users"},
	}

	for _, d := range defaults {
		_, err := s.GetGroup(ctx, d.name)
		if err == nil {
			continue // Already exists
		}
		if !errors.Is(err, models.ErrGroupNotFound) {
			return created, err
		}

		group := &models.Group{
			Name:        d.name,
			GID:         d.gid,
			Description: d.description,
		}
		if _, err := s.CreateGroup(ctx, group); err != nil {
			return created, err
		}
		created = true
	}

	// Add admin user to admins group if both exist
	if _, err := s.GetUser(ctx, models.AdminUsername); err == nil {
		if _, err := s.GetGroup(ctx, "admins"); err == nil {
			// Ignore error - user might already be in group
			_ = s.AddUserToGroup(ctx, models.AdminUsername, "admins")
		}
	}

	return created, nil
}
