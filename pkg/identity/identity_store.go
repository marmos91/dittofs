package identity

import (
	"context"
	"time"
)

// IdentityStore provides persistent user, group, and identity mapping operations.
// This is the control plane persistence layer for user management, separate from
// the data plane metadata stores used for filesystem operations.
//
// Thread Safety: Implementations must be safe for concurrent use from multiple
// goroutines, as methods may be called concurrently from API handlers and
// protocol adapters.
type IdentityStore interface {
	// User read operations

	// GetUser returns a user by username.
	// Returns ErrUserNotFound if the user doesn't exist.
	GetUser(username string) (*User, error)

	// GetUserByID returns a user by their unique ID (UUID).
	// Returns ErrUserNotFound if no user has this ID.
	GetUserByID(id string) (*User, error)

	// ListUsers returns all users.
	ListUsers() ([]*User, error)

	// ValidateCredentials verifies username/password credentials.
	// Returns the user if credentials are valid.
	// Returns ErrInvalidCredentials if the credentials are invalid.
	// Returns ErrUserDisabled if the user account is disabled.
	ValidateCredentials(username, password string) (*User, error)

	// User write operations

	// CreateUser creates a new user.
	// The user ID should be set by the implementation if empty.
	// Returns ErrDuplicateUser if a user with the same username exists.
	CreateUser(ctx context.Context, user *User) error

	// UpdateUser updates an existing user.
	// Returns ErrUserNotFound if the user doesn't exist.
	UpdateUser(ctx context.Context, user *User) error

	// DeleteUser deletes a user by username.
	// Returns ErrUserNotFound if the user doesn't exist.
	// Also deletes all share identity mappings for the user.
	DeleteUser(ctx context.Context, username string) error

	// UpdatePassword updates a user's password hash and NT hash.
	// This is separate from UpdateUser to allow password-only updates.
	// Returns ErrUserNotFound if the user doesn't exist.
	UpdatePassword(ctx context.Context, username, passwordHash, ntHash string) error

	// UpdateLastLogin updates the user's last login timestamp.
	// Returns ErrUserNotFound if the user doesn't exist.
	UpdateLastLogin(ctx context.Context, username string, timestamp time.Time) error

	// Group read operations

	// GetGroup returns a group by name.
	// Returns ErrGroupNotFound if the group doesn't exist.
	GetGroup(name string) (*Group, error)

	// ListGroups returns all groups.
	ListGroups() ([]*Group, error)

	// GetUserGroups returns all groups a user belongs to.
	// Returns ErrUserNotFound if the user doesn't exist.
	GetUserGroups(username string) ([]*Group, error)

	// Group write operations

	// CreateGroup creates a new group.
	// Returns ErrDuplicateGroup if a group with the same name exists.
	CreateGroup(ctx context.Context, group *Group) error

	// UpdateGroup updates an existing group.
	// Returns ErrGroupNotFound if the group doesn't exist.
	UpdateGroup(ctx context.Context, group *Group) error

	// DeleteGroup deletes a group by name.
	// Returns ErrGroupNotFound if the group doesn't exist.
	// Users belonging to the group are updated to remove the group reference.
	DeleteGroup(ctx context.Context, name string) error

	// AddUserToGroup adds a user to a group.
	// Returns ErrUserNotFound if the user doesn't exist.
	// Returns ErrGroupNotFound if the group doesn't exist.
	// No error if the user is already in the group.
	AddUserToGroup(ctx context.Context, username, groupName string) error

	// RemoveUserFromGroup removes a user from a group.
	// Returns ErrUserNotFound if the user doesn't exist.
	// No error if the user was not in the group.
	RemoveUserFromGroup(ctx context.Context, username, groupName string) error

	// Share identity mapping operations

	// GetShareIdentityMapping returns the identity mapping for a user on a share.
	// Returns ErrUserNotFound if the user doesn't exist.
	// Returns nil (no error) if no mapping exists for the share.
	GetShareIdentityMapping(username, shareName string) (*ShareIdentityMapping, error)

	// SetShareIdentityMapping creates or updates a share identity mapping.
	// Returns ErrUserNotFound if the user doesn't exist.
	SetShareIdentityMapping(ctx context.Context, mapping *ShareIdentityMapping) error

	// DeleteShareIdentityMapping deletes a share identity mapping.
	// Returns ErrUserNotFound if the user doesn't exist.
	// No error if the mapping didn't exist.
	DeleteShareIdentityMapping(ctx context.Context, username, shareName string) error

	// ListUserShareMappings returns all share mappings for a user.
	// Returns ErrUserNotFound if the user doesn't exist.
	ListUserShareMappings(username string) ([]*ShareIdentityMapping, error)

	// Permission resolution

	// ResolveSharePermission returns the effective permission for a user on a share.
	// Resolution order: user explicit > group permissions (highest wins) > default
	ResolveSharePermission(user *User, shareName string, defaultPerm SharePermission) SharePermission

	// Guest access

	// GetGuestUser returns the guest user if guest access is enabled.
	// Returns ErrGuestDisabled if guest access is not configured.
	GetGuestUser() (*User, error)

	// IsGuestEnabled returns whether guest access is enabled.
	IsGuestEnabled() bool

	// Admin initialization

	// EnsureAdminUser ensures an admin user exists.
	// If no admin user exists, creates one with the provided or generated password.
	// Returns the initial password if a new admin was created, empty string otherwise.
	// This should be called during server startup.
	EnsureAdminUser(ctx context.Context) (initialPassword string, err error)

	// IsAdminInitialized returns whether the admin user has been initialized.
	// Returns true if an admin user exists.
	IsAdminInitialized(ctx context.Context) (bool, error)

	// Health

	// Healthcheck verifies the store is operational.
	// Returns an error if the store is not healthy.
	Healthcheck(ctx context.Context) error

	// Close closes the store and releases resources.
	Close() error
}
