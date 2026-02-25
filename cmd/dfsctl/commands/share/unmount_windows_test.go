//go:build windows

package share

import (
	"testing"
)

func TestValidateUnmountPoint_Empty(t *testing.T) {
	err := validateUnmountPoint("")
	if err == nil {
		t.Fatal("validateUnmountPoint(\"\") expected error, got nil")
	}
}

func TestValidateUnmountPoint_Valid(t *testing.T) {
	tests := []struct {
		name       string
		mountPoint string
	}{
		{"drive letter", "Z:"},
		{"path", `C:\mnt\share`},
		{"simple string", "something"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateUnmountPoint(tt.mountPoint); err != nil {
				t.Errorf("validateUnmountPoint(%q) returned unexpected error: %v", tt.mountPoint, err)
			}
		})
	}
}

func TestCheckUnmountPrivileges(t *testing.T) {
	if err := checkUnmountPrivileges("Z:"); err != nil {
		t.Errorf("checkUnmountPrivileges() returned unexpected error: %v", err)
	}
}
