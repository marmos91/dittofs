package metadata

import (
	"sync"
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
}

// TestIdentity_HasGID_Concurrent asserts HasGID is safe on an Identity shared by
// several goroutines, as the SMB per-session identity cache does: one *Identity
// is handed to every concurrent request on the session.
func TestIdentity_HasGID_Concurrent(t *testing.T) {
	t.Parallel()

	gids := make([]uint32, 64)
	for i := range gids {
		gids[i] = uint32(i) + 100
	}
	identity := &Identity{GIDs: gids}

	const goroutines = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for _, want := range gids {
				assert.True(t, identity.HasGID(want))
				assert.False(t, identity.HasGID(want+1_000_000))
			}
		}()
	}
	close(start)
	wg.Wait()
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

	// Populates only the scalar fields, so it cannot detect a field the copy
	// drops. TestCopyFileAttr_CopiesEveryField in copy_file_attr_test.go walks
	// the struct reflectively and is the one that covers completeness.
	t.Run("copies the scalar fields", func(t *testing.T) {
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
			PayloadID:    "content-123",
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
		assert.Equal(t, original.PayloadID, result.PayloadID)
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
