package metadata

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Identity.HasGID Tests
// ============================================================================

func TestIdentity_HasGID(t *testing.T) {
	t.Parallel()

	t.Run("empty GIDs returns false", func(t *testing.T) {
		t.Parallel()
		identity := &Identity{GIDs: nil}

		assert.False(t, identity.HasGID(1000))
	})

	t.Run("empty slice returns false", func(t *testing.T) {
		t.Parallel()
		identity := &Identity{GIDs: []uint32{}}

		assert.False(t, identity.HasGID(1000))
	})

	t.Run("GID present returns true", func(t *testing.T) {
		t.Parallel()
		identity := &Identity{GIDs: []uint32{100, 200, 300}}

		assert.True(t, identity.HasGID(200))
	})

	t.Run("GID not present returns false", func(t *testing.T) {
		t.Parallel()
		identity := &Identity{GIDs: []uint32{100, 200, 300}}

		assert.False(t, identity.HasGID(999))
	})

	t.Run("first GID found", func(t *testing.T) {
		t.Parallel()
		identity := &Identity{GIDs: []uint32{100, 200, 300}}

		assert.True(t, identity.HasGID(100))
	})

	t.Run("last GID found", func(t *testing.T) {
		t.Parallel()
		identity := &Identity{GIDs: []uint32{100, 200, 300}}

		assert.True(t, identity.HasGID(300))
	})

	t.Run("lazy map initialization", func(t *testing.T) {
		t.Parallel()
		identity := &Identity{GIDs: []uint32{100, 200, 300}}

		// Map should be nil initially
		assert.Nil(t, identity.gidSet)

		// First call should initialize map
		_ = identity.HasGID(200)

		// Map should now be populated
		assert.NotNil(t, identity.gidSet)
		assert.Len(t, identity.gidSet, 3)
	})

	t.Run("subsequent calls use cached map", func(t *testing.T) {
		t.Parallel()
		identity := &Identity{GIDs: []uint32{100, 200, 300}}

		// Multiple calls should work correctly
		assert.True(t, identity.HasGID(100))
		assert.True(t, identity.HasGID(200))
		assert.True(t, identity.HasGID(300))
		assert.False(t, identity.HasGID(400))
	})
}

// ============================================================================
// ApplyIdentityMapping Tests
// ============================================================================

func TestApplyIdentityMapping(t *testing.T) {
	t.Parallel()

	anonUID := uint32(65534)
	anonGID := uint32(65534)
	anonSID := "S-1-5-7"

	t.Run("nil mapping returns original", func(t *testing.T) {
		t.Parallel()
		identity := &Identity{
			UID:      Uint32Ptr(1000),
			GID:      Uint32Ptr(1000),
			Username: "testuser",
		}

		result := ApplyIdentityMapping(identity, nil)

		assert.Same(t, identity, result) // Should return same pointer
	})

	t.Run("MapAllToAnonymous maps all fields", func(t *testing.T) {
		t.Parallel()
		identity := &Identity{
			UID:       Uint32Ptr(1000),
			GID:       Uint32Ptr(1000),
			GIDs:      []uint32{100, 200},
			SID:       StringPtr("S-1-5-21-123-456-789-1001"),
			GroupSIDs: []string{"S-1-5-32-545"},
			Username:  "testuser",
			Domain:    "EXAMPLE",
		}
		mapping := &IdentityMapping{
			MapAllToAnonymous: true,
			AnonymousUID:      &anonUID,
			AnonymousGID:      &anonGID,
			AnonymousSID:      &anonSID,
		}

		result := ApplyIdentityMapping(identity, mapping)

		require.NotNil(t, result.UID)
		assert.Equal(t, anonUID, *result.UID)
		require.NotNil(t, result.GID)
		assert.Equal(t, anonGID, *result.GID)
		require.NotNil(t, result.SID)
		assert.Equal(t, anonSID, *result.SID)
		assert.Nil(t, result.GIDs)
		assert.Nil(t, result.GroupSIDs)
	})

	t.Run("MapPrivilegedToAnonymous maps root user", func(t *testing.T) {
		t.Parallel()
		identity := &Identity{
			UID:  Uint32Ptr(0), // root
			GID:  Uint32Ptr(0),
			GIDs: []uint32{0, 100},
		}
		mapping := &IdentityMapping{
			MapPrivilegedToAnonymous: true,
			AnonymousUID:             &anonUID,
			AnonymousGID:             &anonGID,
		}

		result := ApplyIdentityMapping(identity, mapping)

		require.NotNil(t, result.UID)
		assert.Equal(t, anonUID, *result.UID)
		require.NotNil(t, result.GID)
		assert.Equal(t, anonGID, *result.GID)
		assert.Nil(t, result.GIDs)
	})

	t.Run("MapPrivilegedToAnonymous preserves non-root user", func(t *testing.T) {
		t.Parallel()
		identity := &Identity{
			UID:  Uint32Ptr(1000),
			GID:  Uint32Ptr(1000),
			GIDs: []uint32{100, 200},
		}
		mapping := &IdentityMapping{
			MapPrivilegedToAnonymous: true,
			AnonymousUID:             &anonUID,
			AnonymousGID:             &anonGID,
		}

		result := ApplyIdentityMapping(identity, mapping)

		require.NotNil(t, result.UID)
		assert.Equal(t, uint32(1000), *result.UID)
		require.NotNil(t, result.GID)
		assert.Equal(t, uint32(1000), *result.GID)
		assert.Equal(t, []uint32{100, 200}, result.GIDs)
	})

	t.Run("MapPrivilegedToAnonymous maps administrator SID", func(t *testing.T) {
		t.Parallel()
		adminSID := "S-1-5-32-544" // BUILTIN\Administrators
		identity := &Identity{
			SID:       &adminSID,
			GroupSIDs: []string{"S-1-5-32-545"},
		}
		mapping := &IdentityMapping{
			MapPrivilegedToAnonymous: true,
			AnonymousSID:             &anonSID,
		}

		result := ApplyIdentityMapping(identity, mapping)

		require.NotNil(t, result.SID)
		assert.Equal(t, anonSID, *result.SID)
		assert.Nil(t, result.GroupSIDs)
	})

	t.Run("deep copy preserves original", func(t *testing.T) {
		t.Parallel()
		identity := &Identity{
			UID:       Uint32Ptr(0),
			GID:       Uint32Ptr(0),
			GIDs:      []uint32{100, 200},
			GroupSIDs: []string{"S-1-5-32-545"},
		}
		mapping := &IdentityMapping{
			MapPrivilegedToAnonymous: true,
			AnonymousUID:             &anonUID,
			AnonymousGID:             &anonGID,
		}

		result := ApplyIdentityMapping(identity, mapping)

		// Original should be unchanged
		assert.Equal(t, uint32(0), *identity.UID)
		assert.Equal(t, uint32(0), *identity.GID)
		assert.Equal(t, []uint32{100, 200}, identity.GIDs)
		assert.Equal(t, []string{"S-1-5-32-545"}, identity.GroupSIDs)

		// Result should be different
		assert.NotEqual(t, *identity.UID, *result.UID)
	})

	t.Run("preserves Username and Domain", func(t *testing.T) {
		t.Parallel()
		identity := &Identity{
			UID:      Uint32Ptr(1000),
			GID:      Uint32Ptr(1000),
			Username: "testuser",
			Domain:   "EXAMPLE",
		}
		mapping := &IdentityMapping{} // No actual mapping

		result := ApplyIdentityMapping(identity, mapping)

		assert.Equal(t, "testuser", result.Username)
		assert.Equal(t, "EXAMPLE", result.Domain)
	})
}

// ============================================================================
// IsAdministratorSID Tests
// ============================================================================

func TestIsAdministratorSID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sid  string
		want bool
	}{
		{"empty string", "", false},
		{"BUILTIN Administrators", "S-1-5-32-544", true},
		{"domain admin 500", "S-1-5-21-3623811015-3361044348-30300820-500", true},
		{"domain admin with different numbers", "S-1-5-21-1-2-3-500", true},
		{"domain user 1001", "S-1-5-21-3623811015-3361044348-30300820-1001", false},
		{"anonymous logon", "S-1-5-7", false},
		{"users group", "S-1-5-32-545", false},
		{"invalid format", "invalid-sid", false},
		{"partial match", "S-1-5-21-500", false}, // Missing 3 sub-authorities
		{"wrong suffix", "S-1-5-21-1-2-3-501", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := IsAdministratorSID(tt.sid)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ============================================================================
// MatchesIPPattern Tests
// ============================================================================

func TestMatchesIPPattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		clientIP string
		pattern  string
		want     bool
	}{
		// Exact match
		{"exact IPv4 match", "192.168.1.100", "192.168.1.100", true},
		{"exact IPv4 no match", "192.168.1.100", "192.168.1.101", false},
		{"exact IPv6 match", "::1", "::1", true},

		// CIDR match
		{"CIDR /24 match", "192.168.1.100", "192.168.1.0/24", true},
		{"CIDR /24 no match", "192.168.2.100", "192.168.1.0/24", false},
		{"CIDR /16 match", "192.168.5.100", "192.168.0.0/16", true},
		{"CIDR /32 match", "192.168.1.100", "192.168.1.100/32", true},
		{"CIDR /8 match", "10.0.0.1", "10.0.0.0/8", true},

		// IPv6 CIDR
		{"IPv6 CIDR match", "2001:db8::1", "2001:db8::/32", true},
		{"IPv6 CIDR no match", "2001:db9::1", "2001:db8::/32", false},

		// Edge cases
		{"invalid client IP with CIDR", "not-an-ip", "192.168.1.0/24", false},
		{"invalid pattern exact", "192.168.1.100", "not-an-ip", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := MatchesIPPattern(tt.clientIP, tt.pattern)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ============================================================================
// CalculatePermissionsFromBits Tests
// ============================================================================

func TestCalculatePermissionsFromBits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		bits uint32
		want Permission
	}{
		{
			"no permissions",
			0,
			0,
		},
		{
			"read only (4)",
			0x4,
			PermissionRead | PermissionListDirectory,
		},
		{
			"write only (2)",
			0x2,
			PermissionWrite | PermissionDelete,
		},
		{
			"execute only (1)",
			0x1,
			PermissionExecute | PermissionTraverse,
		},
		{
			"read-write (6)",
			0x6,
			PermissionRead | PermissionListDirectory | PermissionWrite | PermissionDelete,
		},
		{
			"read-execute (5)",
			0x5,
			PermissionRead | PermissionListDirectory | PermissionExecute | PermissionTraverse,
		},
		{
			"write-execute (3)",
			0x3,
			PermissionWrite | PermissionDelete | PermissionExecute | PermissionTraverse,
		},
		{
			"all permissions (7)",
			0x7,
			PermissionRead | PermissionListDirectory | PermissionWrite | PermissionDelete | PermissionExecute | PermissionTraverse,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := CalculatePermissionsFromBits(tt.bits)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ============================================================================
// CheckOtherPermissions Tests
// ============================================================================

func TestCheckOtherPermissions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mode      uint32
		requested Permission
		want      Permission
	}{
		{
			"mode 0777 grants all to other",
			0777,
			PermissionRead | PermissionWrite | PermissionExecute,
			PermissionRead | PermissionWrite | PermissionExecute,
		},
		{
			"mode 0700 grants nothing to other",
			0700,
			PermissionRead | PermissionWrite | PermissionExecute,
			0,
		},
		{
			"mode 0755 grants read-execute to other",
			0755,
			PermissionRead | PermissionWrite | PermissionExecute,
			PermissionRead | PermissionExecute,
		},
		{
			"mode 0644 grants read to other",
			0644,
			PermissionRead | PermissionWrite,
			PermissionRead,
		},
		{
			"only returns requested permissions",
			0777,
			PermissionRead,
			PermissionRead,
		},
		{
			"mode 0001 grants execute to other",
			0001,
			PermissionExecute,
			PermissionExecute,
		},
		{
			"mode 0002 grants write to other",
			0002,
			PermissionWrite,
			PermissionWrite,
		},
		{
			"mode 0004 grants read to other",
			0004,
			PermissionRead,
			PermissionRead,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := CheckOtherPermissions(tt.mode, tt.requested)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ============================================================================
// CopyFileAttr Tests
// ============================================================================

func TestCopyFileAttr(t *testing.T) {
	t.Parallel()

	t.Run("nil returns nil", func(t *testing.T) {
		t.Parallel()
		result := CopyFileAttr(nil)
		assert.Nil(t, result)
	})

	t.Run("copies all fields", func(t *testing.T) {
		t.Parallel()
		now := time.Now()
		original := &FileAttr{
			Type:         FileTypeRegular,
			Mode:         0644,
			UID:          1000,
			GID:          1000,
			Nlink:        1,
			Size:         1024,
			Atime:        now,
			Mtime:        now,
			Ctime:        now,
			CreationTime: now,
			ContentID:    "content-123",
			LinkTarget:   "/path/to/target",
			Rdev:         0,
			Hidden:       true,
		}

		result := CopyFileAttr(original)

		require.NotNil(t, result)
		assert.NotSame(t, original, result) // Different pointer
		assert.Equal(t, original.Type, result.Type)
		assert.Equal(t, original.Mode, result.Mode)
		assert.Equal(t, original.UID, result.UID)
		assert.Equal(t, original.GID, result.GID)
		assert.Equal(t, original.Nlink, result.Nlink)
		assert.Equal(t, original.Size, result.Size)
		assert.Equal(t, original.Atime, result.Atime)
		assert.Equal(t, original.Mtime, result.Mtime)
		assert.Equal(t, original.Ctime, result.Ctime)
		assert.Equal(t, original.CreationTime, result.CreationTime)
		assert.Equal(t, original.ContentID, result.ContentID)
		assert.Equal(t, original.LinkTarget, result.LinkTarget)
		assert.Equal(t, original.Rdev, result.Rdev)
		assert.Equal(t, original.Hidden, result.Hidden)
	})

	t.Run("modifications to copy do not affect original", func(t *testing.T) {
		t.Parallel()
		original := &FileAttr{
			Type: FileTypeRegular,
			Mode: 0644,
			UID:  1000,
		}

		result := CopyFileAttr(original)
		result.Mode = 0755
		result.UID = 2000

		assert.Equal(t, uint32(0644), original.Mode)
		assert.Equal(t, uint32(1000), original.UID)
	})
}

// ============================================================================
// calculatePermissions Tests
// ============================================================================

func TestCalculatePermissions(t *testing.T) {
	t.Parallel()

	t.Run("nil identity gets other permissions only", func(t *testing.T) {
		t.Parallel()
		file := &File{
			FileAttr: FileAttr{
				Mode: 0754, // owner=rwx, group=r-x, other=r--
			},
		}

		// Request all permissions including ListDirectory to test what we get
		requested := PermissionRead | PermissionListDirectory | PermissionWrite | PermissionExecute
		result := calculatePermissions(file, nil, nil, requested)

		// Other has read only (4) which maps to Read|ListDirectory
		assert.Equal(t, PermissionRead|PermissionListDirectory, result)
	})

	t.Run("identity with nil UID gets other permissions", func(t *testing.T) {
		t.Parallel()
		file := &File{
			FileAttr: FileAttr{
				Mode: 0754,
			},
		}
		identity := &Identity{UID: nil}

		// Request all permissions including ListDirectory
		requested := PermissionRead | PermissionListDirectory | PermissionWrite | PermissionExecute
		result := calculatePermissions(file, identity, nil, requested)

		assert.Equal(t, PermissionRead|PermissionListDirectory, result)
	})

	t.Run("root (UID 0) gets all requested permissions", func(t *testing.T) {
		t.Parallel()
		file := &File{
			FileAttr: FileAttr{
				Mode: 0000, // No permissions for anyone
				UID:  1000,
				GID:  1000,
			},
		}
		identity := &Identity{
			UID: Uint32Ptr(0), // root
			GID: Uint32Ptr(0),
		}

		result := calculatePermissions(file, identity, nil, PermissionRead|PermissionWrite|PermissionExecute)

		assert.Equal(t, PermissionRead|PermissionWrite|PermissionExecute, result)
	})

	t.Run("root on read-only share cannot write", func(t *testing.T) {
		t.Parallel()
		file := &File{
			FileAttr: FileAttr{
				Mode: 0777,
			},
		}
		identity := &Identity{
			UID: Uint32Ptr(0),
			GID: Uint32Ptr(0),
		}
		shareOpts := &ShareOptions{ReadOnly: true}

		// Request all permissions including ListDirectory
		requested := PermissionRead | PermissionListDirectory | PermissionWrite | PermissionDelete
		result := calculatePermissions(file, identity, shareOpts, requested)

		// Read-only share removes write/delete permissions even for root
		assert.Equal(t, PermissionRead|PermissionListDirectory, result)
	})

	t.Run("owner gets owner permission bits", func(t *testing.T) {
		t.Parallel()
		file := &File{
			FileAttr: FileAttr{
				Mode: 0700, // owner=rwx, group=---, other=---
				UID:  1000,
				GID:  1000,
			},
		}
		identity := &Identity{
			UID: Uint32Ptr(1000),
			GID: Uint32Ptr(1000),
		}

		// Request all possible permissions
		requested := PermissionRead | PermissionListDirectory | PermissionWrite | PermissionDelete |
			PermissionExecute | PermissionTraverse | PermissionChangePermissions | PermissionChangeOwnership
		result := calculatePermissions(file, identity, nil, requested)

		// Owner bits (7) + owner privileges
		expected := PermissionRead | PermissionListDirectory | PermissionWrite | PermissionDelete |
			PermissionExecute | PermissionTraverse | PermissionChangePermissions | PermissionChangeOwnership
		assert.Equal(t, expected, result)
	})

	t.Run("owner gets change permissions privilege", func(t *testing.T) {
		t.Parallel()
		file := &File{
			FileAttr: FileAttr{
				Mode: 0600,
				UID:  1000,
				GID:  1000,
			},
		}
		identity := &Identity{
			UID: Uint32Ptr(1000),
			GID: Uint32Ptr(1000),
		}

		result := calculatePermissions(file, identity, nil, PermissionChangePermissions|PermissionChangeOwnership)

		assert.Equal(t, PermissionChangePermissions|PermissionChangeOwnership, result)
	})

	t.Run("group member gets group permission bits", func(t *testing.T) {
		t.Parallel()
		file := &File{
			FileAttr: FileAttr{
				Mode: 0070, // owner=---, group=rwx, other=---
				UID:  1000,
				GID:  2000,
			},
		}
		identity := &Identity{
			UID: Uint32Ptr(3000), // Not owner
			GID: Uint32Ptr(2000), // Primary GID matches file GID
		}

		// Request all the permissions we expect to get back
		requested := PermissionRead | PermissionListDirectory | PermissionWrite | PermissionDelete |
			PermissionExecute | PermissionTraverse
		result := calculatePermissions(file, identity, nil, requested)

		// Mode 070 (rwx for group) grants all these permissions
		expected := PermissionRead | PermissionListDirectory | PermissionWrite | PermissionDelete |
			PermissionExecute | PermissionTraverse
		assert.Equal(t, expected, result)
	})

	t.Run("supplementary group member gets group permissions", func(t *testing.T) {
		t.Parallel()
		file := &File{
			FileAttr: FileAttr{
				Mode: 0070,
				UID:  1000,
				GID:  2000,
			},
		}
		identity := &Identity{
			UID:  Uint32Ptr(3000),
			GID:  Uint32Ptr(3000),      // Different primary group
			GIDs: []uint32{2000, 4000}, // Supplementary includes file GID
		}

		// Request all the permissions we expect to get back
		requested := PermissionRead | PermissionListDirectory | PermissionWrite | PermissionDelete |
			PermissionExecute | PermissionTraverse
		result := calculatePermissions(file, identity, nil, requested)

		expected := PermissionRead | PermissionListDirectory | PermissionWrite | PermissionDelete |
			PermissionExecute | PermissionTraverse
		assert.Equal(t, expected, result)
	})

	t.Run("non-owner non-group gets other bits", func(t *testing.T) {
		t.Parallel()
		file := &File{
			FileAttr: FileAttr{
				Mode: 0007, // owner=---, group=---, other=rwx
				UID:  1000,
				GID:  1000,
			},
		}
		identity := &Identity{
			UID:  Uint32Ptr(2000),
			GID:  Uint32Ptr(2000),
			GIDs: []uint32{3000, 4000}, // None match file GID
		}

		// Request all the permissions we expect to get back
		requested := PermissionRead | PermissionListDirectory | PermissionWrite | PermissionDelete |
			PermissionExecute | PermissionTraverse
		result := calculatePermissions(file, identity, nil, requested)

		expected := PermissionRead | PermissionListDirectory | PermissionWrite | PermissionDelete |
			PermissionExecute | PermissionTraverse
		assert.Equal(t, expected, result)
	})

	t.Run("read-only share blocks write for non-root", func(t *testing.T) {
		t.Parallel()
		file := &File{
			FileAttr: FileAttr{
				Mode: 0777,
				UID:  1000,
				GID:  1000,
			},
		}
		identity := &Identity{
			UID: Uint32Ptr(1000),
			GID: Uint32Ptr(1000),
		}
		shareOpts := &ShareOptions{ReadOnly: true}

		// Request read, write, delete and also changePermissions/changeOwnership
		requested := PermissionRead | PermissionWrite | PermissionDelete |
			PermissionChangePermissions | PermissionChangeOwnership | PermissionListDirectory
		result := calculatePermissions(file, identity, shareOpts, requested)

		// Write and Delete should be blocked, but Read and owner privileges remain
		assert.Equal(t, PermissionRead|PermissionListDirectory|PermissionChangePermissions|PermissionChangeOwnership, result)
	})

	t.Run("only returns requested permissions", func(t *testing.T) {
		t.Parallel()
		file := &File{
			FileAttr: FileAttr{
				Mode: 0777,
				UID:  1000,
				GID:  1000,
			},
		}
		identity := &Identity{
			UID: Uint32Ptr(1000),
			GID: Uint32Ptr(1000),
		}

		// When requesting only PermissionRead, result is intersected with requested
		result := calculatePermissions(file, identity, nil, PermissionRead)

		// Only PermissionRead is returned because that's all we requested
		assert.Equal(t, PermissionRead, result)
	})
}

// ============================================================================
// Permission Constants Tests
// ============================================================================

func TestPermissionConstants(t *testing.T) {
	t.Parallel()

	t.Run("permissions are unique powers of 2", func(t *testing.T) {
		t.Parallel()
		permissions := []Permission{
			PermissionRead,
			PermissionWrite,
			PermissionExecute,
			PermissionDelete,
			PermissionListDirectory,
			PermissionTraverse,
			PermissionChangePermissions,
			PermissionChangeOwnership,
		}

		seen := make(map[Permission]bool)
		for _, p := range permissions {
			// Each should be a power of 2
			assert.True(t, p > 0 && (p&(p-1)) == 0, "Permission %d is not a power of 2", p)
			// Each should be unique
			assert.False(t, seen[p], "Permission %d is duplicated", p)
			seen[p] = true
		}
	})

	t.Run("permissions can be combined", func(t *testing.T) {
		t.Parallel()
		combined := PermissionRead | PermissionWrite | PermissionExecute

		assert.True(t, combined&PermissionRead != 0)
		assert.True(t, combined&PermissionWrite != 0)
		assert.True(t, combined&PermissionExecute != 0)
		assert.False(t, combined&PermissionDelete != 0)
	})
}

// ============================================================================
// Helper Functions
// ============================================================================

// StringPtr returns a pointer to a string value.
func StringPtr(s string) *string {
	return &s
}
