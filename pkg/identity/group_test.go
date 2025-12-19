package identity

import (
	"testing"
)

func TestGroup_GetSID(t *testing.T) {
	tests := []struct {
		name     string
		group    Group
		wantAuto bool
	}{
		{
			name:     "returns explicit SID when set",
			group:    Group{Name: "admins", GID: 100, SID: "S-1-5-21-123-456-789"},
			wantAuto: false,
		},
		{
			name:     "auto-generates SID when not set",
			group:    Group{Name: "admins", GID: 100, SID: ""},
			wantAuto: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.group.GetSID()
			if tc.wantAuto {
				expected := GenerateGroupSIDFromGID(tc.group.GID)
				if result != expected {
					t.Errorf("GetSID() = %q, want auto-generated %q", result, expected)
				}
			} else {
				if result != tc.group.SID {
					t.Errorf("GetSID() = %q, want explicit %q", result, tc.group.SID)
				}
			}
		})
	}
}

func TestGroup_GetSharePermission(t *testing.T) {
	group := Group{
		Name: "editors",
		GID:  101,
		SharePermissions: map[string]SharePermission{
			"/export":  PermissionReadWrite,
			"/private": PermissionRead,
		},
	}

	tests := []struct {
		name      string
		shareName string
		expected  SharePermission
	}{
		{"existing share /export", "/export", PermissionReadWrite},
		{"existing share /private", "/private", PermissionRead},
		{"non-existent share", "/unknown", PermissionNone},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := group.GetSharePermission(tc.shareName)
			if result != tc.expected {
				t.Errorf("GetSharePermission(%q) = %q, want %q", tc.shareName, result, tc.expected)
			}
		})
	}

	// Test with nil SharePermissions
	t.Run("nil share permissions", func(t *testing.T) {
		emptyGroup := Group{Name: "empty", GID: 999}
		result := emptyGroup.GetSharePermission("/any")
		if result != PermissionNone {
			t.Errorf("GetSharePermission() = %q, want %q for nil map", result, PermissionNone)
		}
	})
}

func TestGroup_Validate(t *testing.T) {
	tests := []struct {
		name    string
		group   Group
		wantErr bool
	}{
		{
			name:    "valid group",
			group:   Group{Name: "admins", GID: 100},
			wantErr: false,
		},
		{
			name:    "empty name",
			group:   Group{Name: "", GID: 100},
			wantErr: true,
		},
		{
			name: "invalid share permission",
			group: Group{
				Name: "admins",
				GID:  100,
				SharePermissions: map[string]SharePermission{
					"/export": "invalid-permission",
				},
			},
			wantErr: true,
		},
		{
			name: "valid share permissions",
			group: Group{
				Name: "admins",
				GID:  100,
				SharePermissions: map[string]SharePermission{
					"/export":  PermissionAdmin,
					"/private": PermissionReadWrite,
				},
			},
			wantErr: false,
		},
		{
			name: "nil share permissions is valid",
			group: Group{
				Name: "admins",
				GID:  100,
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.group.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestGenerateGroupSIDFromGID(t *testing.T) {
	tests := []struct {
		name string
		gid  uint32
	}{
		{"GID 0", 0},
		{"GID 100", 100},
		{"GID 65534", 65534},
		{"max GID", 4294967295},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sid := GenerateGroupSIDFromGID(tc.gid)

			// Verify format starts with expected prefix
			if len(sid) < 10 {
				t.Errorf("GenerateGroupSIDFromGID(%d) = %q, too short", tc.gid, sid)
			}

			// Verify determinism - same GID should always generate same SID
			sid2 := GenerateGroupSIDFromGID(tc.gid)
			if sid != sid2 {
				t.Errorf("GenerateGroupSIDFromGID(%d) not deterministic: %q != %q", tc.gid, sid, sid2)
			}
		})
	}

	// Verify different GIDs produce different SIDs
	t.Run("uniqueness", func(t *testing.T) {
		sid1 := GenerateGroupSIDFromGID(100)
		sid2 := GenerateGroupSIDFromGID(101)
		if sid1 == sid2 {
			t.Errorf("Different GIDs produced same SID: %q", sid1)
		}
	})

	// Verify group SIDs are different from user SIDs with same ID
	t.Run("group vs user SID difference", func(t *testing.T) {
		groupSID := GenerateGroupSIDFromGID(1000)
		userSID := GenerateSIDFromUID(1000)
		if groupSID == userSID {
			t.Errorf("Group and user SIDs should be different for same ID: %q", groupSID)
		}
	})
}

func TestWellKnownGroups(t *testing.T) {
	expectedGroups := map[string]uint32{
		"admins":  100,
		"editors": 101,
		"viewers": 102,
	}

	for name, gid := range expectedGroups {
		t.Run(name, func(t *testing.T) {
			if WellKnownGroups[name] != gid {
				t.Errorf("WellKnownGroups[%q] = %d, want %d", name, WellKnownGroups[name], gid)
			}
		})
	}

	if len(WellKnownGroups) != len(expectedGroups) {
		t.Errorf("WellKnownGroups has %d entries, want %d", len(WellKnownGroups), len(expectedGroups))
	}
}
