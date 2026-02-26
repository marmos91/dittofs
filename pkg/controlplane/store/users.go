package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ============================================
// USER OPERATIONS
// ============================================

func (s *GORMStore) GetUser(ctx context.Context, username string) (*models.User, error) {
	return getByField[models.User](s.db, ctx, "username", username, models.ErrUserNotFound, "Groups", "SharePermissions")
}

func (s *GORMStore) GetUserByID(ctx context.Context, id string) (*models.User, error) {
	return getByField[models.User](s.db, ctx, "id", id, models.ErrUserNotFound, "Groups", "SharePermissions")
}

func (s *GORMStore) GetUserByUID(ctx context.Context, uid uint32) (*models.User, error) {
	return getByField[models.User](s.db, ctx, "uid", uid, models.ErrUserNotFound, "Groups", "SharePermissions")
}

func (s *GORMStore) ListUsers(ctx context.Context) ([]*models.User, error) {
	return listAll[models.User](s.db, ctx, "Groups", "SharePermissions")
}

func (s *GORMStore) CreateUser(ctx context.Context, user *models.User) (string, error) {
	user.CreatedAt = time.Now()
	return createWithID(s.db, ctx, user, func(u *models.User, id string) { u.ID = id }, user.ID, models.ErrDuplicateUser)
}

func (s *GORMStore) UpdateUser(ctx context.Context, user *models.User) error {
	// Check if user exists first
	var existing models.User
	if err := s.db.WithContext(ctx).Where("id = ?", user.ID).First(&existing).Error; err != nil {
		return convertNotFoundError(err, models.ErrUserNotFound)
	}

	// Update specific fields using Select to handle pointers properly
	return s.db.WithContext(ctx).
		Model(&existing).
		Select("Username", "Enabled", "MustChangePassword", "Role", "UID", "GID", "DisplayName", "Email").
		Updates(user).Error
}

func (s *GORMStore) DeleteUser(ctx context.Context, username string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user models.User
		if err := tx.Where("username = ?", username).First(&user).Error; err != nil {
			return convertNotFoundError(err, models.ErrUserNotFound)
		}

		// Delete share permissions
		if err := tx.Where("user_id = ?", user.ID).Delete(&models.UserSharePermission{}).Error; err != nil {
			return err
		}

		// Remove from groups (GORM handles the join table)
		if err := tx.Model(&user).Association("Groups").Clear(); err != nil {
			return err
		}

		// Delete user
		if err := tx.Delete(&user).Error; err != nil {
			return err
		}

		return nil
	})
}

func (s *GORMStore) UpdatePassword(ctx context.Context, username, passwordHash, ntHash string) error {
	result := s.db.WithContext(ctx).
		Model(&models.User{}).
		Where("username = ?", username).
		Updates(map[string]any{
			"password_hash": passwordHash,
			"nt_hash":       ntHash,
		})

	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return models.ErrUserNotFound
	}
	return nil
}

func (s *GORMStore) UpdateLastLogin(ctx context.Context, username string, timestamp time.Time) error {
	result := s.db.WithContext(ctx).
		Model(&models.User{}).
		Where("username = ?", username).
		Update("last_login", timestamp)

	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return models.ErrUserNotFound
	}
	return nil
}

func (s *GORMStore) ValidateCredentials(ctx context.Context, username, password string) (*models.User, error) {
	user, err := s.GetUser(ctx, username)
	if err != nil {
		if errors.Is(err, models.ErrUserNotFound) {
			return nil, models.ErrInvalidCredentials
		}
		return nil, err
	}

	if !user.Enabled {
		return nil, models.ErrUserDisabled
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, models.ErrInvalidCredentials
	}

	return user, nil
}

// ============================================
// ADMIN INITIALIZATION
// ============================================

func (s *GORMStore) EnsureAdminUser(ctx context.Context) (string, error) {
	// Check if admin exists
	_, err := s.GetUser(ctx, models.AdminUsername)
	if err == nil {
		return "", nil // Admin already exists
	}
	if !errors.Is(err, models.ErrUserNotFound) {
		return "", err // Unexpected error
	}

	// Check if password was explicitly set via environment variable
	passwordFromEnv := os.Getenv(models.EnvAdminInitialPassword) != ""

	// Generate or get password from environment
	password, err := models.GetOrGenerateAdminPassword()
	if err != nil {
		return "", fmt.Errorf("failed to generate password: %w", err)
	}

	// Hash password with NT hash for SMB support
	passwordHash, ntHash, err := models.HashPasswordWithNT(password)
	if err != nil {
		return "", fmt.Errorf("failed to hash password: %w", err)
	}

	// Create admin user
	admin := models.DefaultAdminUser(passwordHash, ntHash)

	// If password was explicitly set via env var, don't require change
	if passwordFromEnv {
		admin.MustChangePassword = false
	}

	if _, err := s.CreateUser(ctx, admin); err != nil {
		return "", fmt.Errorf("failed to create admin user: %w", err)
	}

	return password, nil
}

func (s *GORMStore) IsAdminInitialized(ctx context.Context) (bool, error) {
	_, err := s.GetUser(ctx, models.AdminUsername)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, models.ErrUserNotFound) {
		return false, nil
	}
	return false, err
}
