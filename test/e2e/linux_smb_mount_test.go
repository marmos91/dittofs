//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/adapter/smb"
	memorycontent "github.com/marmos91/dittofs/pkg/blocks/store/memory"
	"github.com/marmos91/dittofs/pkg/identity"
	"github.com/marmos91/dittofs/pkg/metadata"
	memorymeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/registry"
	"github.com/marmos91/dittofs/pkg/server"
	"github.com/marmos91/dittofs/test/e2e/framework"
)

// TestLinuxSMBMount tests SMB mount.cifs compatibility with Linux kernel CIFS driver.
// This test uses Docker to run a Linux container that mounts the SMB share.
// It verifies that the SMB2 protocol implementation works correctly with mount.cifs.
func TestLinuxSMBMount(t *testing.T) {
	// Skip on non-Linux/macOS systems where Docker might not be available
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("Skipping Linux SMB mount test on %s", runtime.GOOS)
	}

	// Check if Docker is available
	if !isDockerAvailable() {
		t.Skip("Docker not available, skipping Linux SMB mount test")
	}

	// Setup server
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Use ERROR level to keep test output clean
	if level := os.Getenv("DITTOFS_LOGGING_LEVEL"); level != "" {
		logger.SetLevel(level)
	} else {
		logger.SetLevel("ERROR")
	}

	// Find free port
	smbPort := framework.FindFreePort(t)

	// Create stores
	metaStore := memorymeta.NewMemoryMetadataStoreWithDefaults()
	contentStore, err := memorycontent.NewMemoryContentStore(ctx)
	if err != nil {
		t.Fatalf("Failed to create content store: %v", err)
	}

	// Create registry
	reg := registry.NewRegistry(nil)

	// Create test user with authentication
	testUser := &identity.User{
		Username:     "testuser",
		PasswordHash: mustHashPassword("testpass123"),
		UID:          1000,
		GID:          1000,
		Enabled:      true,
		DisplayName:  "Test User",
		SharePermissions: map[string]identity.SharePermission{
			"/export": identity.PermissionReadWrite,
		},
	}

	userStore, err := identity.NewConfigUserStore([]*identity.User{testUser}, nil, nil)
	if err != nil {
		t.Fatalf("Failed to create user store: %v", err)
	}
	reg.SetUserStore(userStore)

	// Register stores
	if err := reg.RegisterMetadataStore("test-metadata", metaStore); err != nil {
		t.Fatalf("Failed to register metadata store: %v", err)
	}
	if err := reg.RegisterContentStore("test-content", contentStore); err != nil {
		t.Fatalf("Failed to register content store: %v", err)
	}

	// Create share
	shareConfig := &registry.ShareConfig{
		Name:              "/export",
		MetadataStore:     "test-metadata",
		ContentStore:      "test-content",
		ReadOnly:          false,
		AllowGuest:        false, // Require authentication
		DefaultPermission: string(identity.PermissionReadWrite),
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0777,
			UID:  0,
			GID:  0,
		},
	}

	if err := reg.AddShare(ctx, shareConfig); err != nil {
		t.Fatalf("Failed to add share: %v", err)
	}

	// Create and start server
	srv := server.New(reg, 30*time.Second)

	smbConfig := smb.SMBConfig{
		Enabled:        true,
		Port:           smbPort,
		MaxConnections: 0,
		Timeouts: smb.SMBTimeoutsConfig{
			Read:     5 * time.Minute,
			Write:    30 * time.Second,
			Idle:     5 * time.Minute,
			Shutdown: 30 * time.Second,
		},
	}
	smbAdapter := smb.New(smbConfig)
	if err := srv.AddAdapter(smbAdapter); err != nil {
		t.Fatalf("Failed to add SMB adapter: %v", err)
	}

	// Start server in background
	go func() {
		if err := srv.Serve(ctx); err != nil && err != context.Canceled {
			t.Logf("Server error: %v", err)
		}
	}()

	// Wait for server to be ready
	framework.WaitForServer(t, smbPort, 10*time.Second)

	// Run Docker tests
	t.Run("MountAndList", func(t *testing.T) {
		testLinuxSMBMountAndList(t, smbPort)
	})

	t.Run("FileSystemInfo", func(t *testing.T) {
		testLinuxSMBFileSystemInfo(t, smbPort)
	})
}

// testLinuxSMBMountAndList tests mount.cifs and directory listing
func testLinuxSMBMountAndList(t *testing.T, port int) {
	script := fmt.Sprintf(`#!/bin/bash
set -e

echo "=== Installing cifs-utils ==="
apt-get update -qq && apt-get install -qq -y cifs-utils > /dev/null 2>&1

echo "=== Creating mount point ==="
mkdir -p /mnt/smb

echo "=== Mounting SMB share ==="
mount -t cifs //host.docker.internal/export /mnt/smb \
    -o username=testuser,password=testpass123,port=%d,vers=2.1

echo "=== Mount successful ==="

echo "=== Directory listing (should be empty or show . and ..) ==="
ls -la /mnt/smb/

echo "=== Creating test file via mount ==="
echo "Hello from mount.cifs!" > /mnt/smb/mount_test.txt

echo "=== Verifying file was created ==="
if [ -f /mnt/smb/mount_test.txt ]; then
    echo "mount_test.txt: FOUND"
    content=$(cat /mnt/smb/mount_test.txt)
    echo "Content: $content"
    if [ "$content" = "Hello from mount.cifs!" ]; then
        echo "Content: CORRECT"
    else
        echo "Content: MISMATCH"
        exit 1
    fi
else
    echo "mount_test.txt: NOT FOUND"
    exit 1
fi

echo "=== Creating test directory ==="
mkdir /mnt/smb/mount_testdir

echo "=== Verifying directory was created ==="
if [ -d /mnt/smb/mount_testdir ]; then
    echo "mount_testdir: FOUND"
else
    echo "mount_testdir: NOT FOUND"
    exit 1
fi

echo "=== Final directory listing ==="
ls -la /mnt/smb/

echo "=== Unmounting ==="
umount /mnt/smb

echo "=== Test PASSED ==="
`, port)

	runDockerScript(t, script, "mount-and-list")
}

// testLinuxSMBFileSystemInfo tests df command
func testLinuxSMBFileSystemInfo(t *testing.T, port int) {
	script := fmt.Sprintf(`#!/bin/bash
set -e

apt-get update -qq && apt-get install -qq -y cifs-utils > /dev/null 2>&1
mkdir -p /mnt/smb

mount -t cifs //host.docker.internal/export /mnt/smb \
    -o username=testuser,password=testpass123,port=%d,vers=2.1

echo "=== Filesystem info (df -h) ==="
df -h /mnt/smb

echo "=== Filesystem type ==="
df -T /mnt/smb

umount /mnt/smb
echo "=== Test PASSED ==="
`, port)

	runDockerScript(t, script, "filesystem-info")
}

// runDockerScript runs a script inside a privileged Ubuntu container
func runDockerScript(t *testing.T, script string, testName string) {
	t.Helper()

	// Create a timeout context for the Docker command
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "run",
		"--rm",
		"--privileged",
		"--add-host=host.docker.internal:host-gateway",
		"ubuntu:22.04",
		"bash", "-c", script)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	// Log output regardless of result
	if stdout.Len() > 0 {
		t.Logf("Docker stdout:\n%s", stdout.String())
	}
	if stderr.Len() > 0 {
		t.Logf("Docker stderr:\n%s", stderr.String())
	}

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			t.Fatalf("Docker test %s timed out after 120s", testName)
		}
		t.Fatalf("Docker test %s failed: %v", testName, err)
	}

	// Verify test passed
	if !strings.Contains(stdout.String(), "Test PASSED") {
		t.Errorf("Test %s did not complete successfully", testName)
	}
}

// isDockerAvailable checks if Docker is available
func isDockerAvailable() bool {
	cmd := exec.Command("docker", "info")
	return cmd.Run() == nil
}

// mustHashPassword hashes a password and panics on error
func mustHashPassword(password string) string {
	hash, err := identity.HashPassword(password)
	if err != nil {
		panic(err)
	}
	return hash
}
