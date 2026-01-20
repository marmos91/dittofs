package identity

import (
	"testing"
)

func TestGroup_GetSharePermission(t *testing.T) {
	group := Group{
		Name: "editors",
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
		emptyGroup := Group{Name: "empty"}
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
			group:   Group{Name: "admins"},
			wantErr: false,
		},
		{
			name:    "empty name",
			group:   Group{Name: ""},
			wantErr: true,
		},
		{
			name: "invalid share permission",
			group: Group{
				Name: "admins",
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
			},
			wantErr: false,
		},
		{
			name: "group with description",
			group: Group{
				Name:        "developers",
				Description: "Development team",
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

func TestWellKnownGroups(t *testing.T) {
	expectedGroups := []string{
		"admins",
		"editors",
		"viewers",
	}

	if len(WellKnownGroups) != len(expectedGroups) {
		t.Errorf("WellKnownGroups has %d entries, want %d", len(WellKnownGroups), len(expectedGroups))
	}

	// Check that expected groups are present
	for _, expected := range expectedGroups {
		found := false
		for _, actual := range WellKnownGroups {
			if actual == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("WellKnownGroups missing %q", expected)
		}
	}
}
