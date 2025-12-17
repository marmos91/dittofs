package metadata

import (
	"testing"
)

func TestApplyIdentityMapping(t *testing.T) {
	uid := uint32(1000)
	gid := uint32(1000)
	anonUID := uint32(65534)
	anonGID := uint32(65534)
	rootUID := uint32(0)
	rootGID := uint32(0)

	tests := []struct {
		name     string
		identity *Identity
		mapping  *IdentityMapping
		wantUID  *uint32
		wantGID  *uint32
		wantSame bool // true if result should be same pointer as input
	}{
		{
			name:     "nil mapping returns original",
			identity: &Identity{UID: &uid, GID: &gid},
			mapping:  nil,
			wantUID:  &uid,
			wantGID:  &gid,
			wantSame: true,
		},
		{
			name:     "map all to anonymous",
			identity: &Identity{UID: &uid, GID: &gid, GIDs: []uint32{100, 200}},
			mapping: &IdentityMapping{
				MapAllToAnonymous: true,
				AnonymousUID:      &anonUID,
				AnonymousGID:      &anonGID,
			},
			wantUID:  &anonUID,
			wantGID:  &anonGID,
			wantSame: false,
		},
		{
			name:     "root squash - root user",
			identity: &Identity{UID: &rootUID, GID: &rootGID, GIDs: []uint32{0}},
			mapping: &IdentityMapping{
				MapPrivilegedToAnonymous: true,
				AnonymousUID:             &anonUID,
				AnonymousGID:             &anonGID,
			},
			wantUID:  &anonUID,
			wantGID:  &anonGID,
			wantSame: false,
		},
		{
			name:     "root squash - non-root user unchanged",
			identity: &Identity{UID: &uid, GID: &gid},
			mapping: &IdentityMapping{
				MapPrivilegedToAnonymous: true,
				AnonymousUID:             &anonUID,
				AnonymousGID:             &anonGID,
			},
			wantUID:  &uid,
			wantGID:  &gid,
			wantSame: false,
		},
		{
			name:     "no mapping flags set - returns copy",
			identity: &Identity{UID: &uid, GID: &gid},
			mapping:  &IdentityMapping{},
			wantUID:  &uid,
			wantGID:  &gid,
			wantSame: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ApplyIdentityMapping(tt.identity, tt.mapping)

			if tt.wantSame {
				if result != tt.identity {
					t.Error("expected same pointer, got different")
				}
			} else {
				if result == tt.identity {
					t.Error("expected different pointer, got same")
				}
			}

			if tt.wantUID != nil {
				if result.UID == nil || *result.UID != *tt.wantUID {
					t.Errorf("UID = %v, want %v", result.UID, tt.wantUID)
				}
			}
			if tt.wantGID != nil {
				if result.GID == nil || *result.GID != *tt.wantGID {
					t.Errorf("GID = %v, want %v", result.GID, tt.wantGID)
				}
			}
		})
	}
}

func TestApplyIdentityMapping_DeepCopy(t *testing.T) {
	uid := uint32(1000)
	gid := uint32(1000)
	originalGIDs := []uint32{100, 200, 300}

	identity := &Identity{
		UID:  &uid,
		GID:  &gid,
		GIDs: originalGIDs,
	}

	mapping := &IdentityMapping{} // No actual mapping, just trigger copy

	result := ApplyIdentityMapping(identity, mapping)

	// Modify the result's GIDs
	if len(result.GIDs) > 0 {
		result.GIDs[0] = 999
	}

	// Original should be unchanged
	if identity.GIDs[0] != 100 {
		t.Errorf("original GIDs modified: got %v, want 100", identity.GIDs[0])
	}
}

func TestIsAdministratorSID(t *testing.T) {
	tests := []struct {
		name string
		sid  string
		want bool
	}{
		{"empty string", "", false},
		{"builtin administrators group", "S-1-5-32-544", true},
		{"domain administrator", "S-1-5-21-3623811015-3361044348-30300820-500", true},
		{"local machine administrator", "S-1-5-21-1234567890-1234567890-1234567890-500", true},
		{"regular user", "S-1-5-21-3623811015-3361044348-30300820-1001", false},
		{"fake admin ending in 500", "S-1-5-21-3623811015-3361044348-30300820-1500", false},
		{"random SID", "S-1-5-18", false},
		{"malformed SID with only 2 sub-authorities", "S-1-5-21-500", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsAdministratorSID(tt.sid)
			if got != tt.want {
				t.Errorf("IsAdministratorSID(%q) = %v, want %v", tt.sid, got, tt.want)
			}
		})
	}
}

func TestMatchesIPPattern(t *testing.T) {
	tests := []struct {
		name     string
		clientIP string
		pattern  string
		want     bool
	}{
		{"exact IPv4 match", "192.168.1.100", "192.168.1.100", true},
		{"exact IPv4 no match", "192.168.1.100", "192.168.1.101", false},
		{"CIDR match", "192.168.1.100", "192.168.1.0/24", true},
		{"CIDR no match", "192.168.2.100", "192.168.1.0/24", false},
		{"IPv6 exact match", "::1", "::1", true},
		{"invalid client IP with CIDR", "invalid", "192.168.1.0/24", false},
		{"empty client IP", "", "192.168.1.0/24", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchesIPPattern(tt.clientIP, tt.pattern)
			if got != tt.want {
				t.Errorf("MatchesIPPattern(%q, %q) = %v, want %v", tt.clientIP, tt.pattern, got, tt.want)
			}
		})
	}
}

func TestCalculatePermissionsFromBits(t *testing.T) {
	tests := []struct {
		name string
		bits uint32
		want Permission
	}{
		{"no permissions", 0, 0},
		{"read only", 0x4, PermissionRead | PermissionListDirectory},
		{"write only", 0x2, PermissionWrite | PermissionDelete},
		{"execute only", 0x1, PermissionExecute | PermissionTraverse},
		{"read-write", 0x6, PermissionRead | PermissionListDirectory | PermissionWrite | PermissionDelete},
		{"all permissions", 0x7, PermissionRead | PermissionListDirectory | PermissionWrite | PermissionDelete | PermissionExecute | PermissionTraverse},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculatePermissionsFromBits(tt.bits)
			if got != tt.want {
				t.Errorf("CalculatePermissionsFromBits(%o) = %v, want %v", tt.bits, got, tt.want)
			}
		})
	}
}

func TestCheckOtherPermissions(t *testing.T) {
	tests := []struct {
		name      string
		mode      uint32
		requested Permission
		want      Permission
	}{
		{"mode 755, request read", 0755, PermissionRead, PermissionRead},
		{"mode 755, request write", 0755, PermissionWrite, 0}, // other has no write
		{"mode 777, request all", 0777, PermissionRead | PermissionWrite | PermissionExecute,
			PermissionRead | PermissionWrite | PermissionExecute},
		{"mode 700, request read", 0700, PermissionRead, 0}, // other has no permissions
		{"mode 644, request execute", 0644, PermissionExecute, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckOtherPermissions(tt.mode, tt.requested)
			if got != tt.want {
				t.Errorf("CheckOtherPermissions(%o, %v) = %v, want %v", tt.mode, tt.requested, got, tt.want)
			}
		})
	}
}

func TestGetInitialLinkCount(t *testing.T) {
	tests := []struct {
		name     string
		fileType FileType
		want     uint32
	}{
		{"regular file", FileTypeRegular, 1},
		{"directory", FileTypeDirectory, 2},
		{"symlink", FileTypeSymlink, 1},
		{"block device", FileTypeBlockDevice, 1},
		{"char device", FileTypeCharDevice, 1},
		{"socket", FileTypeSocket, 1},
		{"fifo", FileTypeFIFO, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetInitialLinkCount(tt.fileType)
			if got != tt.want {
				t.Errorf("GetInitialLinkCount(%v) = %d, want %d", tt.fileType, got, tt.want)
			}
		})
	}
}

func TestCopyFileAttr(t *testing.T) {
	t.Run("nil input", func(t *testing.T) {
		result := CopyFileAttr(nil)
		if result != nil {
			t.Errorf("CopyFileAttr(nil) = %v, want nil", result)
		}
	})

	t.Run("copies all fields", func(t *testing.T) {
		original := &FileAttr{
			Type:       FileTypeRegular,
			Mode:       0644,
			UID:        1000,
			GID:        1000,
			Size:       12345,
			ContentID:  "test-content-id",
			LinkTarget: "/some/target",
		}

		copy := CopyFileAttr(original)

		if copy == original {
			t.Error("CopyFileAttr returned same pointer")
		}

		if copy.Type != original.Type {
			t.Errorf("Type = %v, want %v", copy.Type, original.Type)
		}
		if copy.Mode != original.Mode {
			t.Errorf("Mode = %o, want %o", copy.Mode, original.Mode)
		}
		if copy.UID != original.UID {
			t.Errorf("UID = %d, want %d", copy.UID, original.UID)
		}
		if copy.GID != original.GID {
			t.Errorf("GID = %d, want %d", copy.GID, original.GID)
		}
		if copy.Size != original.Size {
			t.Errorf("Size = %d, want %d", copy.Size, original.Size)
		}
		if copy.ContentID != original.ContentID {
			t.Errorf("ContentID = %s, want %s", copy.ContentID, original.ContentID)
		}
		if copy.LinkTarget != original.LinkTarget {
			t.Errorf("LinkTarget = %s, want %s", copy.LinkTarget, original.LinkTarget)
		}
	})

	t.Run("modifying copy does not affect original", func(t *testing.T) {
		original := &FileAttr{
			Mode: 0644,
			UID:  1000,
		}

		copy := CopyFileAttr(original)
		copy.Mode = 0755
		copy.UID = 2000

		if original.Mode != 0644 {
			t.Errorf("original Mode modified: got %o, want 0644", original.Mode)
		}
		if original.UID != 1000 {
			t.Errorf("original UID modified: got %d, want 1000", original.UID)
		}
	})
}
