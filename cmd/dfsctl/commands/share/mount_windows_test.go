//go:build windows

package share

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

func TestValidateMountPoint_DriveLetters(t *testing.T) {
	tests := []struct {
		name       string
		mountPoint string
		wantErr    bool
	}{
		{"uppercase Z:", "Z:", false},
		{"lowercase z:", "z:", false},
		{"uppercase A:", "A:", false},
		{"digit 1: is invalid", "1:", true},
		{"ZZ falls through to dir check", "ZZ", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMountPoint(tt.mountPoint)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateMountPoint(%q) error = %v, wantErr %v", tt.mountPoint, err, tt.wantErr)
			}
		})
	}
}

func TestValidateMountPoint_Directory(t *testing.T) {
	existingDir := t.TempDir()

	nonExistentDir := filepath.Join(t.TempDir(), "nonexistent")

	tmpFile := filepath.Join(t.TempDir(), "afile.txt")
	if err := os.WriteFile(tmpFile, []byte("hello"), 0644); err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}

	tests := []struct {
		name           string
		mountPoint     string
		wantErr        bool
		errMsgContains string
	}{
		{"existing directory is valid", existingDir, false, ""},
		{"non-existent directory", nonExistentDir, true, "does not exist"},
		{"regular file", tmpFile, true, "not a directory"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMountPoint(tt.mountPoint)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateMountPoint(%q) error = %v, wantErr %v", tt.mountPoint, err, tt.wantErr)
			}
			if err != nil && tt.errMsgContains != "" {
				if !strings.Contains(err.Error(), tt.errMsgContains) {
					t.Errorf("error message %q does not contain %q", err.Error(), tt.errMsgContains)
				}
			}
		})
	}
}

func TestValidatePlatform(t *testing.T) {
	if err := validatePlatform(); err != nil {
		t.Errorf("validatePlatform() returned unexpected error: %v", err)
	}
}

func TestGetDefaultModeForPlatform(t *testing.T) {
	mode, _ := getDefaultModeForPlatform()
	if mode != "" {
		t.Errorf("getDefaultModeForPlatform() mode = %q, want empty string", mode)
	}
}

func TestCheckMountPrivileges(t *testing.T) {
	if err := checkMountPrivileges("/mnt", "nfs", "/export"); err != nil {
		t.Errorf("checkMountPrivileges() returned unexpected error: %v", err)
	}
}

func TestMountNFS_RejectsNonStandardPort(t *testing.T) {
	adapters := []apiclient.Adapter{
		{Type: "nfs", Port: 12049, Enabled: true},
	}

	err := mountNFS("/export", "Z:", adapters)
	if err == nil {
		t.Fatal("mountNFS() expected error for non-standard port, got nil")
	}

	if !strings.Contains(err.Error(), "does not support mounting from a custom port") {
		t.Errorf("error message %q does not mention custom port restriction", err.Error())
	}
}

func TestMountNFS_AcceptsStandardPort(t *testing.T) {
	// Check if the Windows NFS mount command is available.
	// If not, skip the test since we cannot validate beyond port checking.
	if _, err := exec.LookPath("mount"); err != nil {
		t.Skip("mount command not available, skipping")
	}

	adapters := []apiclient.Adapter{
		{Type: "nfs", Port: 2049, Enabled: true},
	}

	err := mountNFS("/export", "Z:", adapters)
	// The mount will likely fail because there is no NFS server running,
	// but the error should NOT be about port validation.
	if err != nil {
		if strings.Contains(err.Error(), "does not support mounting from a custom port") {
			t.Errorf("mountNFS() should not reject standard port 2049, got: %v", err)
		}
	}
}
