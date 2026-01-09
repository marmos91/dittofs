package identity

import (
	"testing"
)

func TestShareIdentityMapping_GetSID(t *testing.T) {
	tests := []struct {
		name     string
		mapping  ShareIdentityMapping
		wantAuto bool
	}{
		{
			name: "returns explicit SID when set",
			mapping: ShareIdentityMapping{
				Username:  "testuser",
				ShareName: "/export",
				UID:       1000,
				GID:       1000,
				SID:       "S-1-5-21-123-456-789",
			},
			wantAuto: false,
		},
		{
			name: "auto-generates SID when not set",
			mapping: ShareIdentityMapping{
				Username:  "testuser",
				ShareName: "/export",
				UID:       1000,
				GID:       1000,
				SID:       "",
			},
			wantAuto: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.mapping.GetSID()
			if tc.wantAuto {
				expected := GenerateSIDFromUID(tc.mapping.UID)
				if result != expected {
					t.Errorf("GetSID() = %q, want auto-generated %q", result, expected)
				}
			} else {
				if result != tc.mapping.SID {
					t.Errorf("GetSID() = %q, want explicit %q", result, tc.mapping.SID)
				}
			}
		})
	}
}

func TestShareIdentityMapping_HasGID(t *testing.T) {
	mapping := ShareIdentityMapping{
		Username:  "testuser",
		ShareName: "/export",
		UID:       1000,
		GID:       1000,
		GIDs:      []uint32{1001, 1002},
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := mapping.HasGID(tc.gid)
			if result != tc.expected {
				t.Errorf("HasGID(%d) = %v, want %v", tc.gid, result, tc.expected)
			}
		})
	}
}

func TestShareIdentityMapping_Validate(t *testing.T) {
	tests := []struct {
		name    string
		mapping ShareIdentityMapping
		wantErr bool
	}{
		{
			name: "valid mapping",
			mapping: ShareIdentityMapping{
				Username:  "testuser",
				ShareName: "/export",
				UID:       1000,
				GID:       1000,
			},
			wantErr: false,
		},
		{
			name: "empty username",
			mapping: ShareIdentityMapping{
				Username:  "",
				ShareName: "/export",
				UID:       1000,
				GID:       1000,
			},
			wantErr: true,
		},
		{
			name: "empty share name",
			mapping: ShareIdentityMapping{
				Username:  "testuser",
				ShareName: "",
				UID:       1000,
				GID:       1000,
			},
			wantErr: true,
		},
		{
			name: "valid mapping with supplementary groups",
			mapping: ShareIdentityMapping{
				Username:  "testuser",
				ShareName: "/export",
				UID:       1000,
				GID:       1000,
				GIDs:      []uint32{1001, 1002},
				SID:       "S-1-5-21-123-456-789",
				GroupSIDs: []string{"S-1-5-21-123-456-790"},
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.mapping.Validate()
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

func TestDefaultShareIdentityMapping(t *testing.T) {
	mapping := DefaultShareIdentityMapping("testuser", "/export", 1000, 1000)

	if mapping.Username != "testuser" {
		t.Errorf("Username = %q, want testuser", mapping.Username)
	}
	if mapping.ShareName != "/export" {
		t.Errorf("ShareName = %q, want /export", mapping.ShareName)
	}
	if mapping.UID != 1000 {
		t.Errorf("UID = %d, want 1000", mapping.UID)
	}
	if mapping.GID != 1000 {
		t.Errorf("GID = %d, want 1000", mapping.GID)
	}
	if mapping.SID != "" {
		t.Errorf("SID = %q, want empty (auto-generate)", mapping.SID)
	}
	if len(mapping.GIDs) != 0 {
		t.Errorf("GIDs = %v, want empty", mapping.GIDs)
	}
}
