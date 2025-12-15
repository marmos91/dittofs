package metadata

import (
	"testing"
	"time"
)

func TestValidateName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		errCode ErrorCode
	}{
		{"empty string", "", true, ErrInvalidArgument},
		{"dot", ".", true, ErrInvalidArgument},
		{"dotdot", "..", true, ErrInvalidArgument},
		{"valid name", "file.txt", false, 0},
		{"valid with spaces", "my file.txt", false, 0},
		{"valid hidden file", ".hidden", false, 0},
		{"valid with dots", "file.tar.gz", false, 0},
		{"valid directory name", "subdir", false, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateName(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ValidateName(%q) = nil, want error", tt.input)
					return
				}
				storeErr, ok := err.(*StoreError)
				if !ok {
					t.Errorf("ValidateName(%q) error is not *StoreError", tt.input)
					return
				}
				if storeErr.Code != tt.errCode {
					t.Errorf("ValidateName(%q) error code = %v, want %v", tt.input, storeErr.Code, tt.errCode)
				}
			} else {
				if err != nil {
					t.Errorf("ValidateName(%q) = %v, want nil", tt.input, err)
				}
			}
		})
	}
}

func TestValidateCreateType(t *testing.T) {
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
			err := ValidateCreateType(tt.fileType)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ValidateCreateType(%v) = nil, want error", tt.fileType)
					return
				}
				storeErr, ok := err.(*StoreError)
				if !ok {
					t.Errorf("ValidateCreateType(%v) error is not *StoreError", tt.fileType)
					return
				}
				if storeErr.Code != ErrInvalidArgument {
					t.Errorf("ValidateCreateType(%v) error code = %v, want %v", tt.fileType, storeErr.Code, ErrInvalidArgument)
				}
			} else {
				if err != nil {
					t.Errorf("ValidateCreateType(%v) = %v, want nil", tt.fileType, err)
				}
			}
		})
	}
}

func TestValidateSpecialFileType(t *testing.T) {
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
			err := ValidateSpecialFileType(tt.fileType)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ValidateSpecialFileType(%v) = nil, want error", tt.fileType)
					return
				}
				storeErr, ok := err.(*StoreError)
				if !ok {
					t.Errorf("ValidateSpecialFileType(%v) error is not *StoreError", tt.fileType)
					return
				}
				if storeErr.Code != ErrInvalidArgument {
					t.Errorf("ValidateSpecialFileType(%v) error code = %v, want %v", tt.fileType, storeErr.Code, ErrInvalidArgument)
				}
			} else {
				if err != nil {
					t.Errorf("ValidateSpecialFileType(%v) = %v, want nil", tt.fileType, err)
				}
			}
		})
	}
}

func TestValidateSymlinkTarget(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{"empty target", "", true},
		{"valid target", "/path/to/file", false},
		{"relative target", "../file", false},
		{"single char", "a", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSymlinkTarget(tt.target)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ValidateSymlinkTarget(%q) = nil, want error", tt.target)
					return
				}
				storeErr, ok := err.(*StoreError)
				if !ok {
					t.Errorf("ValidateSymlinkTarget(%q) error is not *StoreError", tt.target)
					return
				}
				if storeErr.Code != ErrInvalidArgument {
					t.Errorf("ValidateSymlinkTarget(%q) error code = %v, want %v", tt.target, storeErr.Code, ErrInvalidArgument)
				}
			} else {
				if err != nil {
					t.Errorf("ValidateSymlinkTarget(%q) = %v, want nil", tt.target, err)
				}
			}
		})
	}
}

func TestRequiresRoot(t *testing.T) {
	rootUID := uint32(0)
	nonRootUID := uint32(1000)

	tests := []struct {
		name    string
		ctx     *AuthContext
		wantErr bool
	}{
		{
			name:    "nil identity",
			ctx:     &AuthContext{Identity: nil},
			wantErr: true,
		},
		{
			name:    "nil UID",
			ctx:     &AuthContext{Identity: &Identity{UID: nil}},
			wantErr: true,
		},
		{
			name:    "non-root user",
			ctx:     &AuthContext{Identity: &Identity{UID: &nonRootUID}},
			wantErr: true,
		},
		{
			name:    "root user",
			ctx:     &AuthContext{Identity: &Identity{UID: &rootUID}},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RequiresRoot(tt.ctx)
			if tt.wantErr {
				if err == nil {
					t.Errorf("RequiresRoot() = nil, want error")
					return
				}
				storeErr, ok := err.(*StoreError)
				if !ok {
					t.Errorf("RequiresRoot() error is not *StoreError")
					return
				}
				if storeErr.Code != ErrAccessDenied {
					t.Errorf("RequiresRoot() error code = %v, want %v", storeErr.Code, ErrAccessDenied)
				}
			} else {
				if err != nil {
					t.Errorf("RequiresRoot() = %v, want nil", err)
				}
			}
		})
	}
}

func TestDefaultMode(t *testing.T) {
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
			got := DefaultMode(tt.fileType)
			if got != tt.want {
				t.Errorf("DefaultMode(%v) = %o, want %o", tt.fileType, got, tt.want)
			}
		})
	}
}

func TestApplyModeDefault(t *testing.T) {
	tests := []struct {
		name     string
		mode     uint32
		fileType FileType
		want     uint32
	}{
		{"zero mode directory", 0, FileTypeDirectory, 0755},
		{"zero mode file", 0, FileTypeRegular, 0644},
		{"zero mode symlink", 0, FileTypeSymlink, 0777},
		{"explicit mode file", 0600, FileTypeRegular, 0600},
		{"explicit mode directory", 0700, FileTypeDirectory, 0700},
		{"mode with extra bits", 0107777, FileTypeRegular, 07777}, // mask to 0o7777
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ApplyModeDefault(tt.mode, tt.fileType)
			if got != tt.want {
				t.Errorf("ApplyModeDefault(%o, %v) = %o, want %o", tt.mode, tt.fileType, got, tt.want)
			}
		})
	}
}

func TestApplyOwnerDefaults(t *testing.T) {
	uid := uint32(1000)
	gid := uint32(1000)

	tests := []struct {
		name    string
		attr    *FileAttr
		ctx     *AuthContext
		wantUID uint32
		wantGID uint32
	}{
		{
			name:    "nil identity - no change",
			attr:    &FileAttr{UID: 0, GID: 0},
			ctx:     &AuthContext{Identity: nil},
			wantUID: 0,
			wantGID: 0,
		},
		{
			name:    "apply from context when zero",
			attr:    &FileAttr{UID: 0, GID: 0},
			ctx:     &AuthContext{Identity: &Identity{UID: &uid, GID: &gid}},
			wantUID: 1000,
			wantGID: 1000,
		},
		{
			name:    "don't override explicit values",
			attr:    &FileAttr{UID: 500, GID: 500},
			ctx:     &AuthContext{Identity: &Identity{UID: &uid, GID: &gid}},
			wantUID: 500,
			wantGID: 500,
		},
		{
			name:    "apply UID only when GID is set",
			attr:    &FileAttr{UID: 0, GID: 500},
			ctx:     &AuthContext{Identity: &Identity{UID: &uid, GID: &gid}},
			wantUID: 1000,
			wantGID: 500,
		},
		{
			name:    "nil GID in context",
			attr:    &FileAttr{UID: 0, GID: 0},
			ctx:     &AuthContext{Identity: &Identity{UID: &uid, GID: nil}},
			wantUID: 1000,
			wantGID: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ApplyOwnerDefaults(tt.attr, tt.ctx)
			if tt.attr.UID != tt.wantUID {
				t.Errorf("ApplyOwnerDefaults() UID = %d, want %d", tt.attr.UID, tt.wantUID)
			}
			if tt.attr.GID != tt.wantGID {
				t.Errorf("ApplyOwnerDefaults() GID = %d, want %d", tt.attr.GID, tt.wantGID)
			}
		})
	}
}

func TestApplyCreateDefaults(t *testing.T) {
	uid := uint32(1000)
	gid := uint32(1000)
	ctx := &AuthContext{Identity: &Identity{UID: &uid, GID: &gid}}

	t.Run("regular file", func(t *testing.T) {
		attr := &FileAttr{Type: FileTypeRegular, Mode: 0, UID: 0, GID: 0}
		before := time.Now()
		ApplyCreateDefaults(attr, ctx, "")
		after := time.Now()

		if attr.Mode != 0644 {
			t.Errorf("Mode = %o, want %o", attr.Mode, 0644)
		}
		if attr.UID != 1000 {
			t.Errorf("UID = %d, want %d", attr.UID, 1000)
		}
		if attr.GID != 1000 {
			t.Errorf("GID = %d, want %d", attr.GID, 1000)
		}
		if attr.Size != 0 {
			t.Errorf("Size = %d, want %d", attr.Size, 0)
		}
		if attr.Atime.Before(before) || attr.Atime.After(after) {
			t.Error("Atime not set to current time")
		}
		if attr.Mtime.Before(before) || attr.Mtime.After(after) {
			t.Error("Mtime not set to current time")
		}
		if attr.Ctime.Before(before) || attr.Ctime.After(after) {
			t.Error("Ctime not set to current time")
		}
	})

	t.Run("directory", func(t *testing.T) {
		attr := &FileAttr{Type: FileTypeDirectory, Mode: 0, UID: 0, GID: 0}
		ApplyCreateDefaults(attr, ctx, "")

		if attr.Mode != 0755 {
			t.Errorf("Mode = %o, want %o", attr.Mode, 0755)
		}
		if attr.Size != 0 {
			t.Errorf("Size = %d, want %d", attr.Size, 0)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		attr := &FileAttr{Type: FileTypeSymlink, Mode: 0, UID: 0, GID: 0}
		target := "/path/to/target"
		ApplyCreateDefaults(attr, ctx, target)

		if attr.Mode != 0777 {
			t.Errorf("Mode = %o, want %o", attr.Mode, 0777)
		}
		if attr.Size != uint64(len(target)) {
			t.Errorf("Size = %d, want %d", attr.Size, len(target))
		}
	})

	t.Run("explicit mode preserved", func(t *testing.T) {
		attr := &FileAttr{Type: FileTypeRegular, Mode: 0600, UID: 0, GID: 0}
		ApplyCreateDefaults(attr, ctx, "")

		if attr.Mode != 0600 {
			t.Errorf("Mode = %o, want %o", attr.Mode, 0600)
		}
	})
}

func TestNowTimestamp(t *testing.T) {
	before := time.Now()
	got := NowTimestamp()
	after := time.Now()

	if got.Before(before) || got.After(after) {
		t.Error("NowTimestamp() did not return current time")
	}
}
