package identity

import (
	"testing"
)

func TestUser_GetDisplayName(t *testing.T) {
	tests := []struct {
		name     string
		user     User
		expected string
	}{
		{
			name:     "returns display name when set",
			user:     User{Username: "jdoe", DisplayName: "John Doe"},
			expected: "John Doe",
		},
		{
			name:     "returns username when display name empty",
			user:     User{Username: "jdoe", DisplayName: ""},
			expected: "jdoe",
		},
		{
			name:     "returns username when display name not set",
			user:     User{Username: "admin"},
			expected: "admin",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.user.GetDisplayName()
			if result != tc.expected {
				t.Errorf("GetDisplayName() = %q, want %q", result, tc.expected)
			}
		})
	}
}

func TestUser_HasGroup(t *testing.T) {
	user := User{
		Username: "jdoe",
		Groups:   []string{"admins", "editors"},
	}

	tests := []struct {
		name      string
		groupName string
		expected  bool
	}{
		{"member of admins", "admins", true},
		{"member of editors", "editors", true},
		{"not member of viewers", "viewers", false},
		{"empty group name", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := user.HasGroup(tc.groupName)
			if result != tc.expected {
				t.Errorf("HasGroup(%q) = %v, want %v", tc.groupName, result, tc.expected)
			}
		})
	}
}

func TestUser_GetExplicitSharePermission(t *testing.T) {
	user := User{
		Username: "jdoe",
		SharePermissions: map[string]SharePermission{
			"/export":  PermissionReadWrite,
			"/private": PermissionAdmin,
		},
	}

	tests := []struct {
		name       string
		shareName  string
		wantPerm   SharePermission
		wantExists bool
	}{
		{"existing share /export", "/export", PermissionReadWrite, true},
		{"existing share /private", "/private", PermissionAdmin, true},
		{"non-existent share", "/unknown", PermissionNone, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			perm, exists := user.GetExplicitSharePermission(tc.shareName)
			if exists != tc.wantExists {
				t.Errorf("GetExplicitSharePermission(%q) exists = %v, want %v", tc.shareName, exists, tc.wantExists)
			}
			if perm != tc.wantPerm {
				t.Errorf("GetExplicitSharePermission(%q) perm = %q, want %q", tc.shareName, perm, tc.wantPerm)
			}
		})
	}

	// Test with nil SharePermissions
	t.Run("nil share permissions", func(t *testing.T) {
		emptyUser := User{Username: "empty"}
		perm, exists := emptyUser.GetExplicitSharePermission("/any")
		if exists {
			t.Error("GetExplicitSharePermission() exists = true, want false for nil map")
		}
		if perm != PermissionNone {
			t.Errorf("GetExplicitSharePermission() perm = %q, want %q", perm, PermissionNone)
		}
	})
}

func TestUser_Validate(t *testing.T) {
	tests := []struct {
		name    string
		user    User
		wantErr bool
	}{
		{
			name:    "valid user",
			user:    User{Username: "jdoe", Role: RoleUser},
			wantErr: false,
		},
		{
			name:    "empty username",
			user:    User{Username: "", Role: RoleUser},
			wantErr: true,
		},
		{
			name:    "valid admin user",
			user:    User{Username: "admin", Role: RoleAdmin},
			wantErr: false,
		},
		{
			name:    "no role (defaults to empty, valid)",
			user:    User{Username: "jdoe"},
			wantErr: false,
		},
		{
			name:    "invalid role",
			user:    User{Username: "jdoe", Role: "superadmin"},
			wantErr: true,
		},
		{
			name: "invalid share permission",
			user: User{
				Username: "jdoe",
				SharePermissions: map[string]SharePermission{
					"/export": "invalid-permission",
				},
			},
			wantErr: true,
		},
		{
			name: "valid share permissions",
			user: User{
				Username: "jdoe",
				SharePermissions: map[string]SharePermission{
					"/export":  PermissionRead,
					"/private": PermissionAdmin,
				},
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.user.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestUser_IsAdmin(t *testing.T) {
	tests := []struct {
		name     string
		user     User
		expected bool
	}{
		{
			name:     "admin role",
			user:     User{Username: "admin", Role: RoleAdmin},
			expected: true,
		},
		{
			name:     "user role",
			user:     User{Username: "user", Role: RoleUser},
			expected: false,
		},
		{
			name:     "empty role",
			user:     User{Username: "user"},
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.user.IsAdmin()
			if result != tc.expected {
				t.Errorf("IsAdmin() = %v, want %v", result, tc.expected)
			}
		})
	}
}

func TestUserRole_IsValid(t *testing.T) {
	tests := []struct {
		name     string
		role     UserRole
		expected bool
	}{
		{"admin role", RoleAdmin, true},
		{"user role", RoleUser, true},
		{"empty role", "", false},
		{"invalid role", "superadmin", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.role.IsValid()
			if result != tc.expected {
				t.Errorf("IsValid() = %v, want %v", result, tc.expected)
			}
		})
	}
}

func TestComputeNTHash(t *testing.T) {
	// Test that ComputeNTHash produces consistent results
	password := "testpassword"

	hash1 := ComputeNTHash(password)
	hash2 := ComputeNTHash(password)

	if hash1 != hash2 {
		t.Errorf("ComputeNTHash() not deterministic: got different results for same input")
	}

	// Different passwords should produce different hashes
	hash3 := ComputeNTHash("differentpassword")
	if hash1 == hash3 {
		t.Errorf("ComputeNTHash() produced same hash for different passwords")
	}
}

func TestUser_GetNTHash(t *testing.T) {
	// Test with valid NT hash
	user := User{Username: "test"}
	user.SetNTHashFromPassword("password")

	ntHash, ok := user.GetNTHash()
	if !ok {
		t.Error("GetNTHash() returned false for valid hash")
	}

	// Verify ntHash is not all zeros (actual hash has content)
	allZeros := true
	for _, b := range ntHash {
		if b != 0 {
			allZeros = false
			break
		}
	}
	if allZeros {
		t.Error("GetNTHash() returned all zeros, expected actual hash")
	}

	// Test with empty NT hash
	emptyUser := User{Username: "empty"}
	_, ok = emptyUser.GetNTHash()
	if ok {
		t.Error("GetNTHash() returned true for empty hash")
	}
}

func TestUser_SetNTHashFromPassword(t *testing.T) {
	user := User{Username: "test"}

	// Verify NTHash is empty initially
	if user.NTHash != "" {
		t.Error("Expected empty NTHash initially")
	}

	user.SetNTHashFromPassword("password")

	// Verify NTHash is now set
	if user.NTHash == "" {
		t.Error("SetNTHashFromPassword() did not set NTHash")
	}

	// Verify it's a valid hex string of expected length (32 hex chars = 16 bytes)
	if len(user.NTHash) != 32 {
		t.Errorf("SetNTHashFromPassword() produced hash of length %d, want 32", len(user.NTHash))
	}
}
