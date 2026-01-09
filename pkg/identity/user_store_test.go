package identity

import (
	"testing"
)

func createTestStore(t *testing.T) *ConfigUserStore {
	t.Helper()

	hash, _ := HashPassword("password123")

	users := []*User{
		{
			ID:           "user-1",
			Username:     "admin",
			PasswordHash: hash,
			Enabled:      true,
			Role:         RoleAdmin,
			Groups:       []string{"admins"},
			SharePermissions: map[string]SharePermission{
				"/private": PermissionAdmin,
			},
		},
		{
			ID:           "user-2",
			Username:     "editor",
			PasswordHash: hash,
			Enabled:      true,
			Role:         RoleUser,
			Groups:       []string{"editors"},
		},
		{
			ID:           "user-3",
			Username:     "viewer",
			PasswordHash: hash,
			Enabled:      true,
			Role:         RoleUser,
			Groups:       []string{"viewers"},
		},
		{
			ID:           "user-4",
			Username:     "disabled",
			PasswordHash: hash,
			Enabled:      false,
			Role:         RoleUser,
		},
	}

	groups := []*Group{
		{
			Name: "admins",
			SharePermissions: map[string]SharePermission{
				"/export": PermissionAdmin,
			},
		},
		{
			Name: "editors",
			SharePermissions: map[string]SharePermission{
				"/export": PermissionReadWrite,
			},
		},
		{
			Name: "viewers",
			SharePermissions: map[string]SharePermission{
				"/export": PermissionRead,
			},
		},
	}

	guest := &GuestConfig{
		Enabled: true,
		UID:     65534,
		GID:     65534,
		SharePermissions: map[string]SharePermission{
			"/public": PermissionRead,
		},
	}

	store, err := NewConfigUserStore(users, groups, guest)
	if err != nil {
		t.Fatalf("Failed to create test store: %v", err)
	}

	return store
}

func TestConfigUserStore_GetUser(t *testing.T) {
	store := createTestStore(t)

	tests := []struct {
		name     string
		username string
		wantErr  error
	}{
		{"existing user", "admin", nil},
		{"another user", "editor", nil},
		{"non-existent user", "unknown", ErrUserNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			user, err := store.GetUser(tc.username)
			if err != tc.wantErr {
				t.Errorf("GetUser(%q) error = %v, wantErr %v", tc.username, err, tc.wantErr)
			}
			if tc.wantErr == nil && user.Username != tc.username {
				t.Errorf("GetUser(%q) username = %q, want %q", tc.username, user.Username, tc.username)
			}
		})
	}
}

func TestConfigUserStore_ValidateCredentials(t *testing.T) {
	store := createTestStore(t)

	tests := []struct {
		name     string
		username string
		password string
		wantErr  error
	}{
		{"correct credentials", "admin", "password123", nil},
		{"wrong password", "admin", "wrongpassword", ErrInvalidCredentials},
		{"non-existent user", "unknown", "password123", ErrInvalidCredentials},
		{"disabled user", "disabled", "password123", ErrUserDisabled},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			user, err := store.ValidateCredentials(tc.username, tc.password)
			if err != tc.wantErr {
				t.Errorf("ValidateCredentials(%q, %q) error = %v, wantErr %v",
					tc.username, tc.password, err, tc.wantErr)
			}
			if tc.wantErr == nil && user.Username != tc.username {
				t.Errorf("ValidateCredentials(%q, %q) username = %q, want %q",
					tc.username, tc.password, user.Username, tc.username)
			}
		})
	}
}

func TestConfigUserStore_GetGroup(t *testing.T) {
	store := createTestStore(t)

	tests := []struct {
		name    string
		group   string
		wantErr error
	}{
		{"existing group", "admins", nil},
		{"non-existent group", "unknown", ErrGroupNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			group, err := store.GetGroup(tc.group)
			if err != tc.wantErr {
				t.Errorf("GetGroup(%q) error = %v, wantErr %v", tc.group, err, tc.wantErr)
			}
			if tc.wantErr == nil && group.Name != tc.group {
				t.Errorf("GetGroup(%q) name = %q, want %q", tc.group, group.Name, tc.group)
			}
		})
	}
}

func TestConfigUserStore_GetUserGroups(t *testing.T) {
	store := createTestStore(t)

	groups, err := store.GetUserGroups("admin")
	if err != nil {
		t.Fatalf("GetUserGroups(admin) error = %v", err)
	}

	if len(groups) != 1 || groups[0].Name != "admins" {
		t.Errorf("GetUserGroups(admin) = %v, want [admins]", groups)
	}

	_, err = store.GetUserGroups("unknown")
	if err != ErrUserNotFound {
		t.Errorf("GetUserGroups(unknown) error = %v, want ErrUserNotFound", err)
	}
}

func TestConfigUserStore_GetGuestUser(t *testing.T) {
	store := createTestStore(t)

	guest, err := store.GetGuestUser()
	if err != nil {
		t.Fatalf("GetGuestUser() error = %v", err)
	}

	if guest.Username != "guest" {
		t.Errorf("GetGuestUser() username = %q, want guest", guest.Username)
	}
	if guest.DisplayName != "Guest" {
		t.Errorf("GetGuestUser() DisplayName = %q, want Guest", guest.DisplayName)
	}
	if guest.Role != RoleUser {
		t.Errorf("GetGuestUser() Role = %q, want %q", guest.Role, RoleUser)
	}
}

func TestConfigUserStore_GetGuestUser_Disabled(t *testing.T) {
	users := []*User{}
	groups := []*Group{}
	guest := &GuestConfig{Enabled: false}

	store, _ := NewConfigUserStore(users, groups, guest)

	_, err := store.GetGuestUser()
	if err != ErrGuestDisabled {
		t.Errorf("GetGuestUser() error = %v, want ErrGuestDisabled", err)
	}
}

func TestConfigUserStore_ResolveSharePermission(t *testing.T) {
	store := createTestStore(t)

	tests := []struct {
		name        string
		username    string
		shareName   string
		defaultPerm SharePermission
		expected    SharePermission
	}{
		{
			name:        "user explicit permission",
			username:    "admin",
			shareName:   "/private",
			defaultPerm: PermissionNone,
			expected:    PermissionAdmin, // Admin has explicit permission
		},
		{
			name:        "group permission",
			username:    "editor",
			shareName:   "/export",
			defaultPerm: PermissionNone,
			expected:    PermissionReadWrite, // Editor inherits from editors group
		},
		{
			name:        "viewer group permission",
			username:    "viewer",
			shareName:   "/export",
			defaultPerm: PermissionNone,
			expected:    PermissionRead, // Viewer inherits from viewers group
		},
		{
			name:        "admin group permission on export",
			username:    "admin",
			shareName:   "/export",
			defaultPerm: PermissionNone,
			expected:    PermissionAdmin, // Admin inherits from admins group
		},
		{
			name:        "no permission - use default",
			username:    "admin",
			shareName:   "/unknown-share",
			defaultPerm: PermissionRead,
			expected:    PermissionRead, // Falls back to default
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			user, err := store.GetUser(tc.username)
			if err != nil {
				t.Fatalf("GetUser(%q) error = %v", tc.username, err)
			}

			result := store.ResolveSharePermission(user, tc.shareName, tc.defaultPerm)
			if result != tc.expected {
				t.Errorf("ResolveSharePermission(%q, %q, %q) = %q, want %q",
					tc.username, tc.shareName, tc.defaultPerm, result, tc.expected)
			}
		})
	}
}

func TestConfigUserStore_ListUsers(t *testing.T) {
	store := createTestStore(t)

	users, err := store.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers() error = %v", err)
	}

	if len(users) != 4 {
		t.Errorf("ListUsers() len = %d, want 4", len(users))
	}
}

func TestConfigUserStore_ListGroups(t *testing.T) {
	store := createTestStore(t)

	groups, err := store.ListGroups()
	if err != nil {
		t.Fatalf("ListGroups() error = %v", err)
	}

	if len(groups) != 3 {
		t.Errorf("ListGroups() len = %d, want 3", len(groups))
	}
}

func TestNewConfigUserStore_DuplicateUser(t *testing.T) {
	hash, _ := HashPassword("password")
	users := []*User{
		{ID: "1", Username: "admin", PasswordHash: hash, Enabled: true},
		{ID: "2", Username: "admin", PasswordHash: hash, Enabled: true},
	}

	_, err := NewConfigUserStore(users, nil, nil)
	if err != ErrDuplicateUser {
		t.Errorf("NewConfigUserStore() error = %v, want ErrDuplicateUser", err)
	}
}

func TestNewConfigUserStore_DuplicateGroup(t *testing.T) {
	groups := []*Group{
		{Name: "admins"},
		{Name: "admins"},
	}

	_, err := NewConfigUserStore(nil, groups, nil)
	if err != ErrDuplicateGroup {
		t.Errorf("NewConfigUserStore() error = %v, want ErrDuplicateGroup", err)
	}
}

func TestConfigUserStore_IsGuestEnabled(t *testing.T) {
	store := createTestStore(t)

	if !store.IsGuestEnabled() {
		t.Error("IsGuestEnabled() = false, want true")
	}

	// Test with guest disabled
	emptyStore, _ := NewConfigUserStore(nil, nil, &GuestConfig{Enabled: false})
	if emptyStore.IsGuestEnabled() {
		t.Error("IsGuestEnabled() = true, want false for disabled guest")
	}

	// Test with nil guest config
	nilStore, _ := NewConfigUserStore(nil, nil, nil)
	if nilStore.IsGuestEnabled() {
		t.Error("IsGuestEnabled() = true, want false for nil guest config")
	}
}

func TestConfigUserStore_GetGuestSharePermission(t *testing.T) {
	store := createTestStore(t)

	// Test existing share permission
	perm := store.GetGuestSharePermission("/public")
	if perm != PermissionRead {
		t.Errorf("GetGuestSharePermission(/public) = %q, want %q", perm, PermissionRead)
	}

	// Test non-existent share
	perm = store.GetGuestSharePermission("/unknown")
	if perm != PermissionNone {
		t.Errorf("GetGuestSharePermission(/unknown) = %q, want %q", perm, PermissionNone)
	}

	// Test with nil guest config
	nilStore, _ := NewConfigUserStore(nil, nil, nil)
	perm = nilStore.GetGuestSharePermission("/public")
	if perm != PermissionNone {
		t.Errorf("GetGuestSharePermission() with nil guest = %q, want %q", perm, PermissionNone)
	}
}
