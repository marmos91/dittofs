package identity

import (
	"testing"
)

func createTestStore(t *testing.T) *ConfigUserStore {
	t.Helper()

	hash, _ := HashPassword("password123")

	users := []*User{
		{
			Username:     "admin",
			PasswordHash: hash,
			Enabled:      true,
			UID:          1000,
			GID:          100,
			Groups:       []string{"admins"},
			SharePermissions: map[string]SharePermission{
				"/private": PermissionAdmin,
			},
		},
		{
			Username:     "editor",
			PasswordHash: hash,
			Enabled:      true,
			UID:          1001,
			GID:          101,
			Groups:       []string{"editors"},
		},
		{
			Username:     "viewer",
			PasswordHash: hash,
			Enabled:      true,
			UID:          1002,
			GID:          102,
			Groups:       []string{"viewers"},
		},
		{
			Username:     "disabled",
			PasswordHash: hash,
			Enabled:      false,
			UID:          1003,
			GID:          100,
		},
	}

	groups := []*Group{
		{
			Name: "admins",
			GID:  100,
			SharePermissions: map[string]SharePermission{
				"/export": PermissionAdmin,
			},
		},
		{
			Name: "editors",
			GID:  101,
			SharePermissions: map[string]SharePermission{
				"/export": PermissionReadWrite,
			},
		},
		{
			Name: "viewers",
			GID:  102,
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

func TestConfigUserStore_GetUserByUID(t *testing.T) {
	store := createTestStore(t)

	tests := []struct {
		name    string
		uid     uint32
		wantErr error
	}{
		{"admin UID", 1000, nil},
		{"editor UID", 1001, nil},
		{"non-existent UID", 9999, ErrUserNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			user, err := store.GetUserByUID(tc.uid)
			if err != tc.wantErr {
				t.Errorf("GetUserByUID(%d) error = %v, wantErr %v", tc.uid, err, tc.wantErr)
			}
			if tc.wantErr == nil && user.UID != tc.uid {
				t.Errorf("GetUserByUID(%d) UID = %d, want %d", tc.uid, user.UID, tc.uid)
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
	if guest.UID != 65534 {
		t.Errorf("GetGuestUser() UID = %d, want 65534", guest.UID)
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
		{Username: "admin", PasswordHash: hash, Enabled: true, UID: 1000, GID: 100},
		{Username: "admin", PasswordHash: hash, Enabled: true, UID: 1001, GID: 100},
	}

	_, err := NewConfigUserStore(users, nil, nil)
	if err != ErrDuplicateUser {
		t.Errorf("NewConfigUserStore() error = %v, want ErrDuplicateUser", err)
	}
}

func TestNewConfigUserStore_DuplicateUID(t *testing.T) {
	hash, _ := HashPassword("password")
	users := []*User{
		{Username: "admin", PasswordHash: hash, Enabled: true, UID: 1000, GID: 100},
		{Username: "editor", PasswordHash: hash, Enabled: true, UID: 1000, GID: 101},
	}

	_, err := NewConfigUserStore(users, nil, nil)
	if err != ErrDuplicateUID {
		t.Errorf("NewConfigUserStore() error = %v, want ErrDuplicateUID", err)
	}
}

func TestNewConfigUserStore_DuplicateGroup(t *testing.T) {
	groups := []*Group{
		{Name: "admins", GID: 100},
		{Name: "admins", GID: 101},
	}

	_, err := NewConfigUserStore(nil, groups, nil)
	if err != ErrDuplicateGroup {
		t.Errorf("NewConfigUserStore() error = %v, want ErrDuplicateGroup", err)
	}
}

func TestNewConfigUserStore_DuplicateGID(t *testing.T) {
	groups := []*Group{
		{Name: "admins", GID: 100},
		{Name: "editors", GID: 100},
	}

	_, err := NewConfigUserStore(nil, groups, nil)
	if err != ErrDuplicateGID {
		t.Errorf("NewConfigUserStore() error = %v, want ErrDuplicateGID", err)
	}
}
