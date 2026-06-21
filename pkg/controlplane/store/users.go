package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

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

func (s *GORMStore) CreateUserWithGroups(ctx context.Context, user *models.User, groupNames []string) (string, error) {
	if len(groupNames) == 0 {
		return s.CreateUser(ctx, user)
	}

	user.CreatedAt = time.Now()
	if user.ID == "" {
		user.ID = uuid.New().String()
	}

	unique := dedup(groupNames)

	var groups []models.Group
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("name IN ?", unique).Find(&groups).Error; err != nil {
			return err
		}
		if len(groups) != len(unique) {
			found := make(map[string]struct{}, len(groups))
			for _, g := range groups {
				found[g.Name] = struct{}{}
			}
			var missing []string
			for _, name := range unique {
				if _, ok := found[name]; !ok {
					missing = append(missing, name)
				}
			}
			return fmt.Errorf("groups not found: %s: %w", strings.Join(missing, ", "), models.ErrGroupNotFound)
		}

		if err := tx.Create(user).Error; err != nil {
			if isUniqueConstraintError(err) {
				return models.ErrDuplicateUser
			}
			return err
		}

		return tx.Model(user).Association("Groups").Append(&groups)
	})
	if err != nil {
		return "", err
	}

	// Back-populate for the caller (GORM's Append doesn't reliably update in-memory)
	user.Groups = groups
	return user.ID, nil
}

func (s *GORMStore) UpdateUser(ctx context.Context, user *models.User) error {
	// Check if user exists first
	var existing models.User
	if err := s.db.WithContext(ctx).Where("id = ?", user.ID).First(&existing).Error; err != nil {
		return convertNotFoundError(err, models.ErrUserNotFound)
	}

	// Update specific fields using Select to handle pointers properly.
	// SID/GroupSIDs are deliberately NOT in this set: an explicit Select forces
	// the column write even for a zero value, so a caller passing a partial
	// user (GroupSIDs == nil) would silently erase persisted Windows identity.
	// Identity columns are written only through UpdateUserSIDInfo, whose
	// contract is to set exactly those columns.
	return s.db.WithContext(ctx).
		Model(&existing).
		Select("Username", "Enabled", "MustChangePassword", "Role", "UID", "GID", "DisplayName", "Email").
		Updates(user).Error
}

// UpdateUserSIDInfo persists the Windows identity (SID + group SIDs) for a user,
// as resolved by a login flow (Kerberos PAC / NTLMSSP / LDAP, AD-3 #1235).
// It writes only the sid and group_sids columns, so it can never clobber other
// profile fields, and is safe to call with the freshly resolved values without
// first reloading the rest of the user.
func (s *GORMStore) UpdateUserSIDInfo(ctx context.Context, username, sid string, groupSIDs []string) error {
	res := s.db.WithContext(ctx).
		Model(&models.User{}).
		Where("username = ?", username).
		Select("SID", "GroupSIDs").
		Updates(&models.User{SID: sid, GroupSIDs: groupSIDs})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return models.ErrUserNotFound
	}
	return nil
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

func (s *GORMStore) UpdatePasswordAndFlags(ctx context.Context, username, passwordHash, ntHash string, mustChangePassword bool) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&models.User{}).
			Where("username = ?", username).
			Updates(map[string]any{
				"password_hash":        passwordHash,
				"nt_hash":              ntHash,
				"must_change_password": mustChangePassword,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return models.ErrUserNotFound
		}
		return nil
	})
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

func (s *GORMStore) EnsureAdminUser(ctx context.Context, requireInitialPasswordChange bool, configuredPasswordHash string) (string, error) {
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

	// A bcrypt hash supplied via config (admin.password_hash) gives operators a
	// known admin credential without a generated password and without writing a
	// plaintext secret to disk. The plaintext env var takes precedence because
	// it can also derive the NT hash for SMB; a bcrypt hash cannot, so an
	// admin bootstrapped this way can authenticate to the control plane / REST
	// API but not over SMB (use the env var for that).
	if configuredPasswordHash != "" && !passwordFromEnv {
		// Fail fast on a non-bcrypt / malformed hash rather than letting every
		// admin login fail later with an opaque error. bcrypt.Cost parses the
		// hash structure and version ($2a$/$2b$/$2y$ are all accepted).
		if _, err := bcrypt.Cost([]byte(configuredPasswordHash)); err != nil {
			return "", fmt.Errorf("admin.password_hash is not a valid bcrypt hash (%w); "+
				"expected a $2a$/$2b$/$2y$ hash, e.g. from `dfsctl` or `htpasswd -bnBC 10 \"\" <pw>`", err)
		}
		// No NT hash is derivable from a bcrypt hash (SMB admin needs the
		// plaintext env var path). Operator chose the password, so do not force
		// a first-login change.
		admin := models.DefaultAdminUser(configuredPasswordHash, "")
		admin.MustChangePassword = false
		if _, err := s.CreateUser(ctx, admin); err != nil {
			return "", fmt.Errorf("failed to create admin user: %w", err)
		}
		return "", nil
	}

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

	// Don't force the first-login password change when the operator opted out,
	// or when the password was supplied out-of-band via the env var (the
	// operator already chose it, so there's nothing to force a change for).
	if !requireInitialPasswordChange || passwordFromEnv {
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
