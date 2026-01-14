package metadata

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// ValidateName Tests
// ============================================================================

func TestValidateName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr ErrorCode
	}{
		{"empty name", "", ErrInvalidArgument},
		{"dot", ".", ErrInvalidArgument},
		{"double dot", "..", ErrInvalidArgument},
		{"valid simple", "file.txt", 0},
		{"valid with spaces", "my file.txt", 0},
		{"valid hidden file", ".hidden", 0},
		{"valid unicode", "файл.txt", 0},
		{"max length", strings.Repeat("a", MaxNameLen), 0},
		{"exceeds max length", strings.Repeat("a", MaxNameLen+1), ErrNameTooLong},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateName(tt.input)

			if tt.wantErr == 0 {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				var storeErr *StoreError
				require.ErrorAs(t, err, &storeErr)
				assert.Equal(t, tt.wantErr, storeErr.Code)
			}
		})
	}
}

// ============================================================================
// ValidatePath Tests
// ============================================================================

func TestValidatePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr ErrorCode
	}{
		{"empty path", "", 0},
		{"simple path", "/foo/bar", 0},
		{"max length", strings.Repeat("a", MaxPathLen), 0},
		{"exceeds max length", strings.Repeat("a", MaxPathLen+1), ErrNameTooLong},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidatePath(tt.input)

			if tt.wantErr == 0 {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				var storeErr *StoreError
				require.ErrorAs(t, err, &storeErr)
				assert.Equal(t, tt.wantErr, storeErr.Code)
			}
		})
	}
}

// ============================================================================
// ValidateCreateType Tests
// ============================================================================

func TestValidateCreateType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fileType FileType
		wantErr  bool
	}{
		{"regular file", FileTypeRegular, false},
		{"directory", FileTypeDirectory, false},
		{"symlink", FileTypeSymlink, true},
		{"block device", FileTypeBlockDevice, true},
		{"char device", FileTypeCharDevice, true},
		{"socket", FileTypeSocket, true},
		{"fifo", FileTypeFIFO, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateCreateType(tt.fileType)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// ============================================================================
// ValidateSpecialFileType Tests
// ============================================================================

func TestValidateSpecialFileType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fileType FileType
		wantErr  bool
	}{
		{"block device", FileTypeBlockDevice, false},
		{"char device", FileTypeCharDevice, false},
		{"socket", FileTypeSocket, false},
		{"fifo", FileTypeFIFO, false},
		{"regular file", FileTypeRegular, true},
		{"directory", FileTypeDirectory, true},
		{"symlink", FileTypeSymlink, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateSpecialFileType(tt.fileType)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// ============================================================================
// ValidateSymlinkTarget Tests
// ============================================================================

func TestValidateSymlinkTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{"empty target", "", true},
		{"valid target", "/path/to/target", false},
		{"relative target", "../other", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateSymlinkTarget(tt.target)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// ============================================================================
// DefaultMode Tests
// ============================================================================

func TestDefaultMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fileType FileType
		want     uint32
	}{
		{"directory", FileTypeDirectory, 0755},
		{"symlink", FileTypeSymlink, 0777},
		{"regular file", FileTypeRegular, 0644},
		{"block device", FileTypeBlockDevice, 0644},
		{"char device", FileTypeCharDevice, 0644},
		{"socket", FileTypeSocket, 0644},
		{"fifo", FileTypeFIFO, 0644},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := DefaultMode(tt.fileType)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ============================================================================
// ApplyModeDefault Tests
// ============================================================================

func TestApplyModeDefault(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mode     uint32
		fileType FileType
		want     uint32
	}{
		{"zero mode applies default for dir", 0, FileTypeDirectory, 0755},
		{"zero mode applies default for file", 0, FileTypeRegular, 0644},
		{"non-zero mode preserved", 0700, FileTypeRegular, 0700},
		{"mode masked to valid bits", 0o107777, FileTypeRegular, 0o7777},
		{"setuid preserved", 0o4755, FileTypeRegular, 0o4755},
		{"setgid preserved", 0o2755, FileTypeDirectory, 0o2755},
		{"sticky bit preserved", 0o1755, FileTypeDirectory, 0o1755},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ApplyModeDefault(tt.mode, tt.fileType)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ============================================================================
// RequiresRoot Tests
// ============================================================================

func TestRequiresRoot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ctx     *AuthContext
		wantErr bool
	}{
		{
			name: "root user",
			ctx: &AuthContext{
				Identity: &Identity{UID: Uint32Ptr(0)},
			},
			wantErr: false,
		},
		{
			name: "non-root user",
			ctx: &AuthContext{
				Identity: &Identity{UID: Uint32Ptr(1000)},
			},
			wantErr: true,
		},
		{
			name:    "nil identity",
			ctx:     &AuthContext{Identity: nil},
			wantErr: true,
		},
		{
			name: "nil UID",
			ctx: &AuthContext{
				Identity: &Identity{UID: nil},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := RequiresRoot(tt.ctx)

			if tt.wantErr {
				assert.Error(t, err)
				var storeErr *StoreError
				require.ErrorAs(t, err, &storeErr)
				assert.Equal(t, ErrPrivilegeRequired, storeErr.Code)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// ============================================================================
// CheckStickyBitRestriction Tests
// ============================================================================

func TestCheckStickyBitRestriction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		callerUID *uint32
		dirMode   uint32
		dirUID    uint32
		fileUID   uint32
		wantErr   bool
	}{
		{
			name:      "no sticky bit",
			callerUID: Uint32Ptr(1000),
			dirMode:   0755,
			dirUID:    0,
			fileUID:   0,
			wantErr:   false,
		},
		{
			name:      "sticky bit with root caller",
			callerUID: Uint32Ptr(0),
			dirMode:   0755 | ModeSticky,
			dirUID:    1000,
			fileUID:   2000,
			wantErr:   false,
		},
		{
			name:      "sticky bit caller owns file",
			callerUID: Uint32Ptr(1000),
			dirMode:   0755 | ModeSticky,
			dirUID:    2000,
			fileUID:   1000,
			wantErr:   false,
		},
		{
			name:      "sticky bit caller owns dir",
			callerUID: Uint32Ptr(1000),
			dirMode:   0755 | ModeSticky,
			dirUID:    1000,
			fileUID:   2000,
			wantErr:   false,
		},
		{
			name:      "sticky bit other user denied",
			callerUID: Uint32Ptr(3000),
			dirMode:   0755 | ModeSticky,
			dirUID:    1000,
			fileUID:   2000,
			wantErr:   true,
		},
		{
			name:      "sticky bit nil identity denied",
			callerUID: nil,
			dirMode:   0755 | ModeSticky,
			dirUID:    1000,
			fileUID:   2000,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := &AuthContext{}
			if tt.callerUID != nil {
				ctx.Identity = &Identity{UID: tt.callerUID}
			}

			dirAttr := &FileAttr{Mode: tt.dirMode, UID: tt.dirUID}
			fileAttr := &FileAttr{UID: tt.fileUID}

			err := CheckStickyBitRestriction(ctx, dirAttr, fileAttr)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// ============================================================================
// BuildPayloadID Tests
// ============================================================================

func TestBuildPayloadID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		shareName string
		fullPath  string
		want      string
	}{
		{"both with leading slash", "/export", "/file.txt", "export/file.txt"},
		{"share with slash", "/export", "file.txt", "export/file.txt"},
		{"path with slash", "export", "/file.txt", "export/file.txt"},
		{"neither with slash", "export", "file.txt", "export/file.txt"},
		{"nested path", "/export", "/dir/subdir/file.txt", "export/dir/subdir/file.txt"},
		{"empty share", "", "/file.txt", "file.txt"},
		{"empty path", "/export", "", "export"},
		{"both empty", "", "", ""},
		{"root path", "/export", "/", "export"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := BuildPayloadID(tt.shareName, tt.fullPath)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ============================================================================
// ApplyOwnerDefaults Tests
// ============================================================================

func TestApplyOwnerDefaults(t *testing.T) {
	t.Parallel()

	t.Run("sets UID and GID from context", func(t *testing.T) {
		t.Parallel()
		attr := &FileAttr{UID: 0, GID: 0}
		ctx := &AuthContext{
			Identity: &Identity{
				UID: Uint32Ptr(1000),
				GID: Uint32Ptr(1000),
			},
		}

		ApplyOwnerDefaults(attr, ctx)

		assert.Equal(t, uint32(1000), attr.UID)
		assert.Equal(t, uint32(1000), attr.GID)
	})

	t.Run("preserves existing non-zero UID", func(t *testing.T) {
		t.Parallel()
		attr := &FileAttr{UID: 500, GID: 0}
		ctx := &AuthContext{
			Identity: &Identity{
				UID: Uint32Ptr(1000),
				GID: Uint32Ptr(1000),
			},
		}

		ApplyOwnerDefaults(attr, ctx)

		assert.Equal(t, uint32(500), attr.UID)
		assert.Equal(t, uint32(1000), attr.GID)
	})

	t.Run("handles nil identity", func(t *testing.T) {
		t.Parallel()
		attr := &FileAttr{UID: 0, GID: 0}
		ctx := &AuthContext{Identity: nil}

		ApplyOwnerDefaults(attr, ctx)

		assert.Equal(t, uint32(0), attr.UID)
		assert.Equal(t, uint32(0), attr.GID)
	})
}

// ============================================================================
// ApplyCreateDefaults Tests
// ============================================================================

func TestApplyCreateDefaults(t *testing.T) {
	t.Parallel()

	t.Run("sets timestamps and mode for regular file", func(t *testing.T) {
		t.Parallel()
		before := time.Now()
		attr := &FileAttr{Type: FileTypeRegular, Mode: 0}
		ctx := &AuthContext{}

		ApplyCreateDefaults(attr, ctx, "")

		assert.Equal(t, uint32(0644), attr.Mode)
		assert.Equal(t, uint64(0), attr.Size)
		assert.True(t, attr.Atime.After(before) || attr.Atime.Equal(before))
		assert.True(t, attr.Mtime.After(before) || attr.Mtime.Equal(before))
		assert.True(t, attr.Ctime.After(before) || attr.Ctime.Equal(before))
	})

	t.Run("sets size to target length for symlink", func(t *testing.T) {
		t.Parallel()
		attr := &FileAttr{Type: FileTypeSymlink, Mode: 0}
		ctx := &AuthContext{}
		target := "/path/to/target"

		ApplyCreateDefaults(attr, ctx, target)

		assert.Equal(t, uint64(len(target)), attr.Size)
		assert.Equal(t, uint32(0777), attr.Mode)
	})

	t.Run("preserves non-zero mode", func(t *testing.T) {
		t.Parallel()
		attr := &FileAttr{Type: FileTypeRegular, Mode: 0600}
		ctx := &AuthContext{}

		ApplyCreateDefaults(attr, ctx, "")

		assert.Equal(t, uint32(0600), attr.Mode)
	})
}

// ============================================================================
// Pointer Helper Tests
// ============================================================================

func TestPointerHelpers(t *testing.T) {
	t.Parallel()

	t.Run("Uint32Ptr", func(t *testing.T) {
		t.Parallel()
		ptr := Uint32Ptr(42)
		require.NotNil(t, ptr)
		assert.Equal(t, uint32(42), *ptr)
	})

	t.Run("Uint64Ptr", func(t *testing.T) {
		t.Parallel()
		ptr := Uint64Ptr(42)
		require.NotNil(t, ptr)
		assert.Equal(t, uint64(42), *ptr)
	})

	t.Run("TimePtr", func(t *testing.T) {
		t.Parallel()
		now := time.Now()
		ptr := TimePtr(now)
		require.NotNil(t, ptr)
		assert.Equal(t, now, *ptr)
	})

	t.Run("BoolPtr", func(t *testing.T) {
		t.Parallel()
		ptr := BoolPtr(true)
		require.NotNil(t, ptr)
		assert.True(t, *ptr)
	})
}
