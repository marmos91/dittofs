// Package memory provides an in-memory implementation of the IdentityStore interface.
// This implementation is for testing only - all data is lost on restart.
package memory

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/identity"
)

// MemoryIdentityStore is an in-memory implementation of identity.IdentityStore.
// It is thread-safe but ephemeral - all data is lost on restart.
// Use this for testing and development only.
type MemoryIdentityStore struct {
	mu sync.RWMutex

	// User storage
	users     map[string]*identity.User // username -> User
	usersByID map[string]*identity.User // UUID -> User

	// Group storage
	groups map[string]*identity.Group // name -> Group

	// Share identity mappings
	// Key format: "username:shareName"
	shareMappings map[string]*identity.ShareIdentityMapping

	// Guest configuration
	guestEnabled bool
	guestUser    *identity.User

	// Admin initialized flag
	adminInitialized bool
}

// NewMemoryIdentityStore creates a new in-memory identity store.
func NewMemoryIdentityStore() *MemoryIdentityStore {
	return &MemoryIdentityStore{
		users:         make(map[string]*identity.User),
		usersByID:     make(map[string]*identity.User),
		groups:        make(map[string]*identity.Group),
		shareMappings: make(map[string]*identity.ShareIdentityMapping),
		guestEnabled:  false,
	}
}

// shareMappingKey generates a key for the shareMappings map.
func shareMappingKey(username, shareName string) string {
	return username + ":" + shareName
}

// hasShareMappingPrefix checks if a share mapping key belongs to a user.
func hasShareMappingPrefix(key, username string) bool {
	prefix := username + ":"
	return len(key) > len(prefix) && key[:len(prefix)] == prefix
}

// copyUser creates a deep copy of a user to prevent external mutation.
func copyUser(u *identity.User) *identity.User {
	if u == nil {
		return nil
	}
	userCopy := *u
	if u.Groups != nil {
		userCopy.Groups = make([]string, len(u.Groups))
		copy(userCopy.Groups, u.Groups)
	}
	if u.SharePermissions != nil {
		userCopy.SharePermissions = make(map[string]identity.SharePermission)
		for k, v := range u.SharePermissions {
			userCopy.SharePermissions[k] = v
		}
	}
	return &userCopy
}

// copyGroup creates a deep copy of a group to prevent external mutation.
func copyGroup(g *identity.Group) *identity.Group {
	if g == nil {
		return nil
	}
	groupCopy := *g
	if g.SharePermissions != nil {
		groupCopy.SharePermissions = make(map[string]identity.SharePermission)
		for k, v := range g.SharePermissions {
			groupCopy.SharePermissions[k] = v
		}
	}
	return &groupCopy
}

// copyMapping creates a deep copy of a share identity mapping.
func copyMapping(m *identity.ShareIdentityMapping) *identity.ShareIdentityMapping {
	if m == nil {
		return nil
	}
	mappingCopy := *m
	if m.GIDs != nil {
		mappingCopy.GIDs = make([]uint32, len(m.GIDs))
		copy(mappingCopy.GIDs, m.GIDs)
	}
	if m.GroupSIDs != nil {
		mappingCopy.GroupSIDs = make([]string, len(m.GroupSIDs))
		copy(mappingCopy.GroupSIDs, m.GroupSIDs)
	}
	return &mappingCopy
}

// ============================================================================
// User Read Operations
// ============================================================================

// GetUser retrieves a user by username.
func (s *MemoryIdentityStore) GetUser(username string) (*identity.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	user, ok := s.users[username]
	if !ok {
		return nil, identity.ErrUserNotFound
	}
	return copyUser(user), nil
}

// GetUserByID retrieves a user by their UUID.
func (s *MemoryIdentityStore) GetUserByID(id string) (*identity.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	user, ok := s.usersByID[id]
	if !ok {
		return nil, identity.ErrUserNotFound
	}
	return copyUser(user), nil
}

// ListUsers returns all users.
func (s *MemoryIdentityStore) ListUsers() ([]*identity.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	users := make([]*identity.User, 0, len(s.users))
	for _, u := range s.users {
		users = append(users, copyUser(u))
	}
	return users, nil
}

// ValidateCredentials validates a username and password.
// Returns the user if credentials are valid, or an error otherwise.
func (s *MemoryIdentityStore) ValidateCredentials(username, password string) (*identity.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	user, ok := s.users[username]
	if !ok {
		return nil, identity.ErrUserNotFound
	}

	if !user.Enabled {
		return nil, identity.ErrUserDisabled
	}

	if !identity.VerifyPassword(password, user.PasswordHash) {
		return nil, identity.ErrInvalidCredentials
	}

	return copyUser(user), nil
}

// ============================================================================
// User Write Operations
// ============================================================================

// CreateUser creates a new user.
func (s *MemoryIdentityStore) CreateUser(ctx context.Context, user *identity.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.users[user.Username]; exists {
		return identity.ErrDuplicateUser
	}

	// Ensure user has an ID
	if user.ID == "" {
		user.ID = uuid.New().String()
	}

	// Store a copy
	stored := copyUser(user)
	s.users[user.Username] = stored
	s.usersByID[user.ID] = stored

	return nil
}

// UpdateUser updates an existing user.
// Note: Username changes are not supported. The user is looked up by Username,
// and attempting to change it would leave orphaned entries.
func (s *MemoryIdentityStore) UpdateUser(ctx context.Context, user *identity.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.users[user.Username]
	if !ok {
		return identity.ErrUserNotFound
	}

	// Prevent username changes - they would cause inconsistencies
	if user.Username != existing.Username {
		return identity.ErrInvalidOperation
	}

	// Store a copy, preserving the ID
	stored := copyUser(user)
	stored.ID = existing.ID

	// Update maps
	s.users[user.Username] = stored
	s.usersByID[stored.ID] = stored

	return nil
}

// DeleteUser deletes a user by username.
func (s *MemoryIdentityStore) DeleteUser(ctx context.Context, username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, ok := s.users[username]
	if !ok {
		return identity.ErrUserNotFound
	}

	// Remove from both maps
	delete(s.users, username)
	delete(s.usersByID, user.ID)

	// Remove all share mappings for this user.
	// Note: In-memory map deletion is atomic and cannot fail in Go.
	// For persistent stores, this should be done transactionally.
	for key := range s.shareMappings {
		if hasShareMappingPrefix(key, username) {
			delete(s.shareMappings, key)
		}
	}

	return nil
}

// UpdatePassword updates a user's password hash and NT hash.
func (s *MemoryIdentityStore) UpdatePassword(ctx context.Context, username, passwordHash, ntHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, ok := s.users[username]
	if !ok {
		return identity.ErrUserNotFound
	}

	user.PasswordHash = passwordHash
	user.NTHash = ntHash

	return nil
}

// UpdateLastLogin updates the user's last login timestamp.
func (s *MemoryIdentityStore) UpdateLastLogin(ctx context.Context, username string, timestamp time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, ok := s.users[username]
	if !ok {
		return identity.ErrUserNotFound
	}

	user.LastLogin = timestamp

	return nil
}

// ============================================================================
// Group Read Operations
// ============================================================================

// GetGroup retrieves a group by name.
func (s *MemoryIdentityStore) GetGroup(name string) (*identity.Group, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	group, ok := s.groups[name]
	if !ok {
		return nil, identity.ErrGroupNotFound
	}
	return copyGroup(group), nil
}

// ListGroups returns all groups.
func (s *MemoryIdentityStore) ListGroups() ([]*identity.Group, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	groups := make([]*identity.Group, 0, len(s.groups))
	for _, g := range s.groups {
		groups = append(groups, copyGroup(g))
	}
	return groups, nil
}

// GetUserGroups returns all groups that a user belongs to.
func (s *MemoryIdentityStore) GetUserGroups(username string) ([]*identity.Group, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	user, ok := s.users[username]
	if !ok {
		return nil, identity.ErrUserNotFound
	}

	groups := make([]*identity.Group, 0, len(user.Groups))
	for _, groupName := range user.Groups {
		if group, ok := s.groups[groupName]; ok {
			groups = append(groups, copyGroup(group))
		}
	}
	return groups, nil
}

// ============================================================================
// Group Write Operations
// ============================================================================

// CreateGroup creates a new group.
func (s *MemoryIdentityStore) CreateGroup(ctx context.Context, group *identity.Group) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.groups[group.Name]; exists {
		return identity.ErrDuplicateGroup
	}

	s.groups[group.Name] = copyGroup(group)
	return nil
}

// UpdateGroup updates an existing group.
func (s *MemoryIdentityStore) UpdateGroup(ctx context.Context, group *identity.Group) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.groups[group.Name]; !ok {
		return identity.ErrGroupNotFound
	}

	s.groups[group.Name] = copyGroup(group)
	return nil
}

// DeleteGroup deletes a group by name.
func (s *MemoryIdentityStore) DeleteGroup(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.groups[name]; !ok {
		return identity.ErrGroupNotFound
	}

	delete(s.groups, name)

	// Remove group from all users' group lists
	for _, user := range s.users {
		newGroups := make([]string, 0, len(user.Groups))
		for _, g := range user.Groups {
			if g != name {
				newGroups = append(newGroups, g)
			}
		}
		user.Groups = newGroups
	}

	return nil
}

// AddUserToGroup adds a user to a group.
func (s *MemoryIdentityStore) AddUserToGroup(ctx context.Context, username, groupName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, ok := s.users[username]
	if !ok {
		return identity.ErrUserNotFound
	}

	if _, ok := s.groups[groupName]; !ok {
		return identity.ErrGroupNotFound
	}

	// Check if already a member
	for _, g := range user.Groups {
		if g == groupName {
			return nil // Already a member
		}
	}

	user.Groups = append(user.Groups, groupName)
	return nil
}

// RemoveUserFromGroup removes a user from a group.
func (s *MemoryIdentityStore) RemoveUserFromGroup(ctx context.Context, username, groupName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, ok := s.users[username]
	if !ok {
		return identity.ErrUserNotFound
	}

	newGroups := make([]string, 0, len(user.Groups))
	found := false
	for _, g := range user.Groups {
		if g == groupName {
			found = true
		} else {
			newGroups = append(newGroups, g)
		}
	}

	if !found {
		return nil // User wasn't in the group
	}

	user.Groups = newGroups
	return nil
}

// ============================================================================
// Share Identity Mapping Operations
// ============================================================================

// GetShareIdentityMapping retrieves a user's identity mapping for a share.
func (s *MemoryIdentityStore) GetShareIdentityMapping(username, shareName string) (*identity.ShareIdentityMapping, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// First check if user exists
	if _, ok := s.users[username]; !ok {
		return nil, identity.ErrUserNotFound
	}

	key := shareMappingKey(username, shareName)
	mapping, ok := s.shareMappings[key]
	if !ok {
		return nil, nil // No mapping, not an error
	}
	return copyMapping(mapping), nil
}

// SetShareIdentityMapping creates or updates a user's identity mapping for a share.
func (s *MemoryIdentityStore) SetShareIdentityMapping(ctx context.Context, mapping *identity.ShareIdentityMapping) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if user exists
	if _, ok := s.users[mapping.Username]; !ok {
		return identity.ErrUserNotFound
	}

	key := shareMappingKey(mapping.Username, mapping.ShareName)
	s.shareMappings[key] = copyMapping(mapping)
	return nil
}

// DeleteShareIdentityMapping deletes a user's identity mapping for a share.
func (s *MemoryIdentityStore) DeleteShareIdentityMapping(ctx context.Context, username, shareName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if user exists
	if _, ok := s.users[username]; !ok {
		return identity.ErrUserNotFound
	}

	key := shareMappingKey(username, shareName)
	delete(s.shareMappings, key)
	return nil
}

// ListUserShareMappings returns all share identity mappings for a user.
func (s *MemoryIdentityStore) ListUserShareMappings(username string) ([]*identity.ShareIdentityMapping, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Check if user exists
	if _, ok := s.users[username]; !ok {
		return nil, identity.ErrUserNotFound
	}

	var mappings []*identity.ShareIdentityMapping
	for key, mapping := range s.shareMappings {
		if hasShareMappingPrefix(key, username) {
			mappings = append(mappings, copyMapping(mapping))
		}
	}
	return mappings, nil
}

// ============================================================================
// Permission Resolution
// ============================================================================

// ResolveSharePermission resolves the effective permission for a user on a share.
// It checks user-specific permissions first, then group permissions, then returns the default.
func (s *MemoryIdentityStore) ResolveSharePermission(user *identity.User, shareName string, defaultPerm identity.SharePermission) identity.SharePermission {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Check user-specific permission first
	if perm, ok := user.SharePermissions[shareName]; ok {
		return perm
	}

	// Check group permissions (highest permission wins)
	highestPerm := identity.PermissionNone
	for _, groupName := range user.Groups {
		if group, ok := s.groups[groupName]; ok {
			if perm, ok := group.SharePermissions[shareName]; ok {
				if perm.Level() > highestPerm.Level() {
					highestPerm = perm
				}
			}
		}
	}

	if highestPerm.Level() > identity.PermissionNone.Level() {
		return highestPerm
	}

	return defaultPerm
}

// ============================================================================
// Guest Access
// ============================================================================

// GetGuestUser returns the guest user if guest access is enabled.
func (s *MemoryIdentityStore) GetGuestUser() (*identity.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if !s.guestEnabled || s.guestUser == nil {
		return nil, identity.ErrUserNotFound
	}
	return copyUser(s.guestUser), nil
}

// IsGuestEnabled returns whether guest access is enabled.
func (s *MemoryIdentityStore) IsGuestEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.guestEnabled
}

// SetGuestUser sets the guest user and enables guest access.
// Pass nil to disable guest access.
func (s *MemoryIdentityStore) SetGuestUser(user *identity.User) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if user == nil {
		s.guestEnabled = false
		s.guestUser = nil
	} else {
		s.guestEnabled = true
		s.guestUser = copyUser(user)
	}
}

// ============================================================================
// Admin Initialization
// ============================================================================

// EnsureAdminUser ensures an admin user exists.
// If the admin user doesn't exist, it creates one with a generated or env-specified password.
// Returns the initial password if a new admin was created, empty string otherwise.
func (s *MemoryIdentityStore) EnsureAdminUser(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if admin already exists
	if _, ok := s.users[identity.AdminUsername]; ok {
		s.adminInitialized = true
		return "", nil
	}

	// Get or generate password
	password, err := identity.GetOrGenerateAdminPassword()
	if err != nil {
		return "", fmt.Errorf("failed to generate admin password: %w", err)
	}

	// Hash the password
	passwordHash, err := identity.HashPassword(password)
	if err != nil {
		return "", err
	}

	// Compute NT hash for SMB
	ntHash := identity.ComputeNTHash(password)

	// Create admin user
	admin := identity.DefaultAdminUser(passwordHash, hex.EncodeToString(ntHash[:]))
	s.users[admin.Username] = admin
	s.usersByID[admin.ID] = admin
	s.adminInitialized = true

	return password, nil
}

// IsAdminInitialized returns whether the admin user has been initialized.
func (s *MemoryIdentityStore) IsAdminInitialized(ctx context.Context) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.adminInitialized, nil
}

// ============================================================================
// Health and Lifecycle
// ============================================================================

// Healthcheck verifies the store is healthy.
// For memory store, this always returns nil.
func (s *MemoryIdentityStore) Healthcheck(ctx context.Context) error {
	return nil
}

// Close releases any resources held by the store.
// For memory store, this is a no-op.
func (s *MemoryIdentityStore) Close() error {
	return nil
}

// Ensure MemoryIdentityStore implements IdentityStore
var _ identity.IdentityStore = (*MemoryIdentityStore)(nil)
