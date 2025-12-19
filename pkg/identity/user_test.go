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

func TestUser_GetSID(t *testing.T) {
	tests := []struct {
		name     string
		user     User
		wantAuto bool
	}{
		{
			name:     "returns explicit SID when set",
			user:     User{UID: 1000, SID: "S-1-5-21-123-456-789"},
			wantAuto: false,
		},
		{
			name:     "auto-generates SID when not set",
			user:     User{UID: 1000, SID: ""},
			wantAuto: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.user.GetSID()
			if tc.wantAuto {
				expected := GenerateSIDFromUID(tc.user.UID)
				if result != expected {
					t.Errorf("GetSID() = %q, want auto-generated %q", result, expected)
				}
			} else {
				if result != tc.user.SID {
					t.Errorf("GetSID() = %q, want explicit %q", result, tc.user.SID)
				}
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

func TestUser_HasGID(t *testing.T) {
	user := User{
		Username: "jdoe",
		GID:      1000,
		GIDs:     []uint32{1001, 1002},
	}

	tests := []struct {
		name     string
		gid      uint32
		expected bool
	}{
		{"primary GID", 1000, true},
		{"supplementary GID 1001", 1001, true},
		{"supplementary GID 1002", 1002, true},
		{"non-member GID", 9999, false},
		{"GID zero", 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := user.HasGID(tc.gid)
			if result != tc.expected {
				t.Errorf("HasGID(%d) = %v, want %v", tc.gid, result, tc.expected)
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
			user:    User{Username: "jdoe", UID: 1000, GID: 1000},
			wantErr: false,
		},
		{
			name:    "empty username",
			user:    User{Username: "", UID: 1000, GID: 1000},
			wantErr: true,
		},
		{
			name:    "UID 0 for non-root",
			user:    User{Username: "jdoe", UID: 0, GID: 1000},
			wantErr: true,
		},
		{
			name:    "UID 0 for root is valid",
			user:    User{Username: "root", UID: 0, GID: 0},
			wantErr: false,
		},
		{
			name: "invalid share permission",
			user: User{
				Username: "jdoe",
				UID:      1000,
				GID:      1000,
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
				UID:      1000,
				GID:      1000,
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

func TestGenerateSIDFromUID(t *testing.T) {
	tests := []struct {
		name string
		uid  uint32
	}{
		{"UID 0", 0},
		{"UID 1000", 1000},
		{"UID 65534", 65534},
		{"max UID", 4294967295},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sid := GenerateSIDFromUID(tc.uid)

			// Verify format starts with expected prefix
			if len(sid) < 10 {
				t.Errorf("GenerateSIDFromUID(%d) = %q, too short", tc.uid, sid)
			}

			// Verify determinism - same UID should always generate same SID
			sid2 := GenerateSIDFromUID(tc.uid)
			if sid != sid2 {
				t.Errorf("GenerateSIDFromUID(%d) not deterministic: %q != %q", tc.uid, sid, sid2)
			}
		})
	}

	// Verify different UIDs produce different SIDs
	t.Run("uniqueness", func(t *testing.T) {
		sid1 := GenerateSIDFromUID(1000)
		sid2 := GenerateSIDFromUID(1001)
		if sid1 == sid2 {
			t.Errorf("Different UIDs produced same SID: %q", sid1)
		}
	})
}

func TestGuestUser(t *testing.T) {
	guest := GuestUser(65534, 65534)

	if guest.Username != "guest" {
		t.Errorf("GuestUser() Username = %q, want guest", guest.Username)
	}
	if !guest.Enabled {
		t.Error("GuestUser() Enabled = false, want true")
	}
	if guest.UID != 65534 {
		t.Errorf("GuestUser() UID = %d, want 65534", guest.UID)
	}
	if guest.GID != 65534 {
		t.Errorf("GuestUser() GID = %d, want 65534", guest.GID)
	}
	if guest.DisplayName != "Guest" {
		t.Errorf("GuestUser() DisplayName = %q, want Guest", guest.DisplayName)
	}
}
