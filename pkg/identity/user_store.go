package identity

import (
	"errors"
	"sync"
)

// Common errors for UserStore operations.
var (
	ErrUserNotFound     = errors.New("user not found")
	ErrGroupNotFound    = errors.New("group not found")
	ErrUserDisabled     = errors.New("user account is disabled")
	ErrGuestDisabled    = errors.New("guest access is disabled")
	ErrDuplicateUser    = errors.New("user already exists")
	ErrDuplicateGroup   = errors.New("group already exists")
	ErrInvalidOperation = errors.New("invalid operation")
)

// UserStore provides user and group management operations.
//
// This interface is DEPRECATED. Use IdentityStore instead for full
// user management capabilities including CRUD operations.
//
// Implementations must be thread-safe as methods may be called
// concurrently from multiple protocol handlers.
type UserStore interface {
	// User operations

	// GetUser returns a user by username.
	// Returns ErrUserNotFound if the user doesn't exist.
	GetUser(username string) (*User, error)

	// ValidateCredentials verifies username/password credentials.
	// Returns ErrInvalidCredentials if the credentials are invalid.
	// Returns ErrUserDisabled if the user account is disabled.
	ValidateCredentials(username, password string) (*User, error)

	// ListUsers returns all users.
	ListUsers() ([]*User, error)

	// GetGuestUser returns the guest user if guest access is enabled.
	// Returns ErrGuestDisabled if guest access is not configured.
	GetGuestUser() (*User, error)

	// Group operations

	// GetGroup returns a group by name.
	// Returns ErrGroupNotFound if the group doesn't exist.
	GetGroup(name string) (*Group, error)

	// ListGroups returns all groups.
	ListGroups() ([]*Group, error)

	// GetUserGroups returns all groups a user belongs to.
	GetUserGroups(username string) ([]*Group, error)

	// Permission resolution

	// ResolveSharePermission returns the effective permission for a user on a share.
	// Resolution order: user explicit > group permissions (highest wins) > default
	ResolveSharePermission(user *User, shareName string, defaultPerm SharePermission) SharePermission
}

// GuestConfig holds configuration for guest/anonymous access.
type GuestConfig struct {
	// Enabled indicates whether guest access is allowed.
	Enabled bool `yaml:"enabled" mapstructure:"enabled"`

	// UID is the Unix user ID for guest operations.
	UID uint32 `yaml:"uid" mapstructure:"uid"`

	// GID is the Unix group ID for guest operations.
	GID uint32 `yaml:"gid" mapstructure:"gid"`

	// SharePermissions maps share names to permission levels for guests.
	SharePermissions map[string]SharePermission `yaml:"share_permissions" mapstructure:"share_permissions"`
}

// ConfigUserStore implements UserStore using in-memory data loaded from configuration.
//
// DEPRECATED: This implementation is for backward compatibility with config-based
// user definitions. Use IdentityStore implementations for new deployments.
//
// Data is loaded from the configuration file at startup and is read-only.
type ConfigUserStore struct {
	mu sync.RWMutex

	// Users indexed by username
	users map[string]*User

	// Groups indexed by name
	groups map[string]*Group

	// Guest configuration
	guest *GuestConfig
}

// NewConfigUserStore creates a new ConfigUserStore with the given users, groups, and guest config.
func NewConfigUserStore(users []*User, groups []*Group, guest *GuestConfig) (*ConfigUserStore, error) {
	store := &ConfigUserStore{
		users:  make(map[string]*User),
		groups: make(map[string]*Group),
		guest:  guest,
	}

	// Index groups first (users reference groups)
	for _, g := range groups {
		if err := g.Validate(); err != nil {
			return nil, err
		}
		if _, exists := store.groups[g.Name]; exists {
			return nil, ErrDuplicateGroup
		}
		store.groups[g.Name] = g
	}

	// Index users
	for _, u := range users {
		if err := u.Validate(); err != nil {
			return nil, err
		}
		if _, exists := store.users[u.Username]; exists {
			return nil, ErrDuplicateUser
		}
		store.users[u.Username] = u
	}

	return store, nil
}

// GetUser returns a user by username.
func (s *ConfigUserStore) GetUser(username string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	user, ok := s.users[username]
	if !ok {
		return nil, ErrUserNotFound
	}
	return user, nil
}

// ValidateCredentials verifies username/password credentials.
func (s *ConfigUserStore) ValidateCredentials(username, password string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	user, ok := s.users[username]
	if !ok {
		return nil, ErrInvalidCredentials
	}

	if !user.Enabled {
		return nil, ErrUserDisabled
	}

	if !VerifyPassword(password, user.PasswordHash) {
		return nil, ErrInvalidCredentials
	}

	return user, nil
}

// ListUsers returns all users.
func (s *ConfigUserStore) ListUsers() ([]*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	users := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		users = append(users, u)
	}
	return users, nil
}

// GetGuestUser returns the guest user if guest access is enabled.
// Note: The returned User does not have UID/GID. Protocol handlers should
// use GuestConfig.UID/GID for Unix identity in file operations.
func (s *ConfigUserStore) GetGuestUser() (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.guest == nil || !s.guest.Enabled {
		return nil, ErrGuestDisabled
	}

	return &User{
		Username:         "guest",
		Enabled:          true,
		Role:             RoleUser,
		SharePermissions: s.guest.SharePermissions,
		DisplayName:      "Guest",
	}, nil
}

// GetGroup returns a group by name.
func (s *ConfigUserStore) GetGroup(name string) (*Group, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	group, ok := s.groups[name]
	if !ok {
		return nil, ErrGroupNotFound
	}
	return group, nil
}

// ListGroups returns all groups.
func (s *ConfigUserStore) ListGroups() ([]*Group, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	groups := make([]*Group, 0, len(s.groups))
	for _, g := range s.groups {
		groups = append(groups, g)
	}
	return groups, nil
}

// GetUserGroups returns all groups a user belongs to.
func (s *ConfigUserStore) GetUserGroups(username string) ([]*Group, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	user, ok := s.users[username]
	if !ok {
		return nil, ErrUserNotFound
	}

	groups := make([]*Group, 0, len(user.Groups))
	for _, groupName := range user.Groups {
		if group, exists := s.groups[groupName]; exists {
			groups = append(groups, group)
		}
	}
	return groups, nil
}

// ResolveSharePermission returns the effective permission for a user on a share.
//
// Resolution order:
//  1. User's explicit permission for the share (highest priority)
//  2. Highest permission from user's groups
//  3. Default permission (lowest priority)
func (s *ConfigUserStore) ResolveSharePermission(user *User, shareName string, defaultPerm SharePermission) SharePermission {
	// 1. Check user's explicit permission
	if perm, ok := user.GetExplicitSharePermission(shareName); ok {
		return perm
	}

	// 2. Check all groups user belongs to, take highest permission
	s.mu.RLock()
	defer s.mu.RUnlock()

	highestPerm := PermissionNone
	for _, groupName := range user.Groups {
		if group, exists := s.groups[groupName]; exists {
			perm := group.GetSharePermission(shareName)
			highestPerm = MaxPermission(highestPerm, perm)
		}
	}

	if highestPerm != PermissionNone {
		return highestPerm
	}

	// 3. Fall back to default
	return defaultPerm
}

// IsGuestEnabled returns whether guest access is enabled.
func (s *ConfigUserStore) IsGuestEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.guest != nil && s.guest.Enabled
}

// GetGuestSharePermission returns the guest permission for a share.
func (s *ConfigUserStore) GetGuestSharePermission(shareName string) SharePermission {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.guest == nil || s.guest.SharePermissions == nil {
		return PermissionNone
	}

	perm, ok := s.guest.SharePermissions[shareName]
	if !ok {
		return PermissionNone
	}
	return perm
}
