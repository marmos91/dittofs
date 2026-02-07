//go:build e2e

package framework

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Mount represents a mounted filesystem (NFS or SMB).
type Mount struct {
	T           *testing.T
	Path        string
	Protocol    string // "nfs" or "smb"
	Port        int
	mounted     bool
	dockerMount *SMBDockerMount // Non-nil if using Docker-based mount
}

// MountNFS mounts an NFS share and returns the mount info.
func MountNFS(t *testing.T, port int) *Mount {
	t.Helper()

	// Give the NFS server a moment to fully initialize
	time.Sleep(500 * time.Millisecond)

	// Create mount directory
	mountPath, err := os.MkdirTemp("", "dittofs-e2e-nfs-*")
	if err != nil {
		t.Fatalf("Failed to create NFS mount directory: %v", err)
	}

	// Build mount command parameters based on platform
	// actimeo=0 disables attribute caching for proper cross-protocol visibility in tests
	mountOptions := fmt.Sprintf("nfsvers=3,tcp,port=%d,mountport=%d,actimeo=0", port, port)
	var mountArgs []string

	switch runtime.GOOS {
	case "darwin":
		mountOptions += ",resvport"
		mountArgs = []string{"-t", "nfs", "-o", mountOptions, "localhost:/export", mountPath}
	case "linux":
		mountOptions += ",nolock"
		mountArgs = []string{"-t", "nfs", "-o", mountOptions, "localhost:/export", mountPath}
	default:
		_ = os.RemoveAll(mountPath)
		t.Fatalf("Unsupported platform for NFS: %s", runtime.GOOS)
	}

	// Execute mount command with retries
	var output []byte
	var lastErr error
	maxRetries := 3

	for i := 0; i < maxRetries; i++ {
		cmd := exec.Command("mount", mountArgs...)
		output, lastErr = cmd.CombinedOutput()

		if lastErr == nil {
			t.Logf("NFS share mounted successfully at %s", mountPath)
			break
		}

		if i < maxRetries-1 {
			t.Logf("NFS mount attempt %d failed (error: %v), retrying in 1 second...", i+1, lastErr)
			time.Sleep(time.Second)
		}
	}

	if lastErr != nil {
		_ = os.RemoveAll(mountPath)
		t.Fatalf("Failed to mount NFS share after %d attempts: %v\nOutput: %s\nMount command: mount %v",
			maxRetries, lastErr, string(output), mountArgs)
	}

	return &Mount{
		T:        t,
		Path:     mountPath,
		Protocol: "nfs",
		Port:     port,
		mounted:  true,
	}
}

// SMBCredentials holds SMB authentication credentials.
type SMBCredentials struct {
	Username string
	Password string
}

// DefaultSMBCredentials returns the default test credentials.
func DefaultSMBCredentials() SMBCredentials {
	return SMBCredentials{
		Username: "testuser",
		Password: "testpass123",
	}
}

// MountSMB mounts an SMB share and returns the mount info.
// It first tries native mount, then falls back to Docker-based mount if available.
func MountSMB(t *testing.T, port int, creds SMBCredentials) *Mount {
	t.Helper()

	// Try native mount first
	mount, err := tryNativeSMBMount(t, port, creds)
	if err == nil {
		return mount
	}

	// Log native mount failure
	t.Logf("Native SMB mount failed: %v, trying Docker-based mount", err)

	// Fallback to Docker-based mount
	if IsDockerAvailable() {
		dockerMount := MountSMBWithDocker(t, port, creds)
		t.Logf("Using Docker-based SMB mount (container: %s)", dockerMount.ContainerID[:12])
		return &Mount{
			T:           t,
			Path:        dockerMount.HostPath,
			Protocol:    "smb",
			Port:        port,
			mounted:     true,
			dockerMount: dockerMount,
		}
	}

	t.Fatalf("SMB mount failed: native mount unavailable (%v) and Docker not available", err)
	return nil
}

// tryNativeSMBMount attempts to mount SMB share using native OS mount command.
// Returns the mount or an error (does not call t.Fatal).
func tryNativeSMBMount(t *testing.T, port int, creds SMBCredentials) (*Mount, error) {
	t.Helper()

	// Give the SMB server a moment to fully initialize
	time.Sleep(500 * time.Millisecond)

	// Create mount directory
	mountPath, err := os.MkdirTemp("", "dittofs-e2e-smb-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create SMB mount directory: %w", err)
	}

	// Build mount command based on platform
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		// macOS: mount_smbfs
		smbURL := fmt.Sprintf("//%s:%s@localhost:%d/export", creds.Username, creds.Password, port)
		cmd = exec.Command("mount_smbfs", smbURL, mountPath)
	case "linux":
		// Linux: mount -t cifs
		cmd = exec.Command("mount", "-t", "cifs",
			"//localhost/export",
			mountPath,
			"-o", fmt.Sprintf("port=%d,username=%s,password=%s,vers=2.1",
				port, creds.Username, creds.Password))
	default:
		_ = os.RemoveAll(mountPath)
		return nil, fmt.Errorf("unsupported platform for SMB: %s", runtime.GOOS)
	}

	// Execute mount command with retries
	var output []byte
	var lastErr error
	maxRetries := 3

	for i := 0; i < maxRetries; i++ {
		output, lastErr = cmd.CombinedOutput()

		if lastErr == nil {
			t.Logf("SMB share mounted successfully at %s (native)", mountPath)
			return &Mount{
				T:        t,
				Path:     mountPath,
				Protocol: "smb",
				Port:     port,
				mounted:  true,
			}, nil
		}

		if i < maxRetries-1 {
			t.Logf("SMB mount attempt %d failed (error: %v), retrying in 1 second...", i+1, lastErr)
			time.Sleep(time.Second)

			// Rebuild command for next attempt (cmd can only be run once)
			switch runtime.GOOS {
			case "darwin":
				smbURL := fmt.Sprintf("//%s:%s@localhost:%d/export", creds.Username, creds.Password, port)
				cmd = exec.Command("mount_smbfs", smbURL, mountPath)
			case "linux":
				cmd = exec.Command("mount", "-t", "cifs",
					"//localhost/export",
					mountPath,
					"-o", fmt.Sprintf("port=%d,username=%s,password=%s,vers=2.1",
						port, creds.Username, creds.Password))
			}
		}
	}

	// Clean up mount directory on failure
	_ = os.RemoveAll(mountPath)
	return nil, fmt.Errorf("failed to mount SMB share after %d attempts: %v\nOutput: %s",
		maxRetries, lastErr, string(output))
}

// MountSMBWithError mounts an SMB share and returns the mount info or an error.
// Unlike MountSMB, this function does NOT call t.Fatal on mount failure.
// Use this for testing scenarios where mount is expected to fail (e.g., permission denied).
func MountSMBWithError(t *testing.T, port int, creds SMBCredentials) (*Mount, error) {
	t.Helper()

	// Give the SMB server a moment to fully initialize
	time.Sleep(500 * time.Millisecond)

	// Create mount directory
	mountPath, err := os.MkdirTemp("", "dittofs-e2e-smb-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create SMB mount directory: %w", err)
	}

	// Build mount command based on platform
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		// macOS: mount_smbfs
		smbURL := fmt.Sprintf("//%s:%s@localhost:%d/export", creds.Username, creds.Password, port)
		cmd = exec.Command("mount_smbfs", smbURL, mountPath)
	case "linux":
		// Linux: mount -t cifs
		cmd = exec.Command("mount", "-t", "cifs",
			"//localhost/export",
			mountPath,
			"-o", fmt.Sprintf("port=%d,username=%s,password=%s,vers=2.1",
				port, creds.Username, creds.Password))
	default:
		_ = os.RemoveAll(mountPath)
		return nil, fmt.Errorf("unsupported platform for SMB: %s", runtime.GOOS)
	}

	// Execute mount command with retries
	var output []byte
	var lastErr error
	maxRetries := 3

	for i := 0; i < maxRetries; i++ {
		output, lastErr = cmd.CombinedOutput()

		if lastErr == nil {
			t.Logf("SMB share mounted successfully at %s", mountPath)
			return &Mount{
				T:        t,
				Path:     mountPath,
				Protocol: "smb",
				Port:     port,
				mounted:  true,
			}, nil
		}

		if i < maxRetries-1 {
			t.Logf("SMB mount attempt %d failed (error: %v), retrying in 1 second...", i+1, lastErr)
			time.Sleep(time.Second)

			// Rebuild command for next attempt (cmd can only be run once)
			switch runtime.GOOS {
			case "darwin":
				smbURL := fmt.Sprintf("//%s:%s@localhost:%d/export", creds.Username, creds.Password, port)
				cmd = exec.Command("mount_smbfs", smbURL, mountPath)
			case "linux":
				cmd = exec.Command("mount", "-t", "cifs",
					"//localhost/export",
					mountPath,
					"-o", fmt.Sprintf("port=%d,username=%s,password=%s,vers=2.1",
						port, creds.Username, creds.Password))
			}
		}
	}

	// Clean up mount directory on failure
	_ = os.RemoveAll(mountPath)
	return nil, fmt.Errorf("failed to mount SMB share after %d attempts: %v\nOutput: %s",
		maxRetries, lastErr, string(output))
}

// Unmount unmounts the filesystem.
func (m *Mount) Unmount() {
	m.T.Helper()

	if !m.mounted || m.Path == "" {
		return
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		// On macOS, use diskutil for cleaner unmount
		cmd = exec.Command("diskutil", "unmount", m.Path)
	case "linux":
		cmd = exec.Command("umount", m.Path)
	default:
		m.T.Logf("Unsupported platform for unmount: %s", runtime.GOOS)
		return
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		m.T.Logf("Failed to unmount %s share: %v\nOutput: %s", m.Protocol, err, string(output))
		// Try force unmount
		switch runtime.GOOS {
		case "darwin":
			cmd = exec.Command("diskutil", "unmount", "force", m.Path)
		default:
			cmd = exec.Command("umount", "-f", m.Path)
		}
		_ = cmd.Run()
	}

	// Wait for the mount to be fully removed from the kernel.
	// On macOS, diskutil returns before the kernel fully releases the mount.
	// We poll until the mount disappears or timeout after 5 seconds.
	waitForUnmount(m.Path, 5*time.Second)

	m.mounted = false
}

// waitForUnmount polls until the path is no longer mounted or timeout expires.
func waitForUnmount(path string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !isMounted(path) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Cleanup unmounts and removes the mount directory.
func (m *Mount) Cleanup() {
	m.T.Helper()

	// Handle Docker-based mount cleanup
	if m.dockerMount != nil {
		m.dockerMount.Cleanup()
		m.mounted = false
		return
	}

	// Native mount cleanup
	m.Unmount()
	if m.Path != "" {
		_ = os.RemoveAll(m.Path)
	}
}

// FilePath returns the absolute path for a relative path within the mount.
// For Docker-based mounts, this still returns the host path since file operations
// go through the shared volume, not the container's internal mount.
func (m *Mount) FilePath(relativePath string) string {
	// Note: For Docker mounts, m.Path is the host path (shared volume),
	// not the container's internal /mnt/smb path. File operations through
	// the standard Go APIs work on the host path.
	return filepath.Join(m.Path, relativePath)
}

// FindFreePort finds an available TCP port.
func FindFreePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to find free port: %v", err)
	}
	defer func() { _ = listener.Close() }()

	return listener.Addr().(*net.TCPAddr).Port
}

// WaitForServer waits for a TCP server to be ready.
func WaitForServer(t *testing.T, port int, timeout time.Duration) {
	t.Helper()

	deadline := time.After(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("Timeout waiting for server on port %d", port)
		case <-ticker.C:
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), time.Second)
			if err == nil {
				_ = conn.Close()
				return
			}
		}
	}
}

// CleanupStaleMounts finds and unmounts any stale DittoFS test mounts.
// This handles cases where tests timeout or panic without proper cleanup.
func CleanupStaleMounts() {
	// Find all dittofs test mount directories in /tmp
	// On macOS, /tmp is a symlink to /private/tmp, so we need to check both paths
	patterns := []string{
		"/tmp/dittofs-test-*",
		"/tmp/dittofs-e2e-*",
		"/tmp/dittofs-e2e-nfs-*",
		"/tmp/dittofs-e2e-smb-*",
		"/tmp/dittofs-e2e-matrix-*",
		"/tmp/dittofs-interop-nfs-*",
		"/tmp/dittofs-interop-smb-*",
		"/tmp/dittofs-smb-*",
		"/tmp/dittofs-cache-*",
		"/tmp/dittofs-shared-*",
	}

	// On macOS, also check /private/tmp (canonical path)
	if runtime.GOOS == "darwin" {
		macOSPatterns := []string{
			"/private/tmp/dittofs-test-*",
			"/private/tmp/dittofs-e2e-*",
			"/private/tmp/dittofs-e2e-nfs-*",
			"/private/tmp/dittofs-e2e-smb-*",
			"/private/tmp/dittofs-e2e-matrix-*",
			"/private/tmp/dittofs-interop-nfs-*",
			"/private/tmp/dittofs-interop-smb-*",
			"/private/tmp/dittofs-smb-*",
			"/private/tmp/dittofs-cache-*",
			"/private/tmp/dittofs-shared-*",
		}
		patterns = append(patterns, macOSPatterns...)
	}

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}

		for _, mountPath := range matches {
			// Check if it's actually mounted
			if isMounted(mountPath) {
				unmountStale(mountPath)
			}

			// Remove the directory if it exists and is empty
			_ = os.Remove(mountPath)
		}
	}
}

// unmountStale attempts to unmount a stale mount point.
func unmountStale(mountPath string) {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		// On macOS, use diskutil for cleaner unmount
		cmd = exec.Command("diskutil", "unmount", mountPath)
	default:
		cmd = exec.Command("umount", mountPath)
	}

	if err := cmd.Run(); err != nil {
		// Force unmount if normal unmount fails
		switch runtime.GOOS {
		case "darwin":
			cmd = exec.Command("diskutil", "unmount", "force", mountPath)
		default:
			cmd = exec.Command("umount", "-f", mountPath)
		}
		_ = cmd.Run()
	}

	// Wait for the mount to be fully removed
	waitForUnmount(mountPath, 3*time.Second)
}

// isMounted checks if a path is currently mounted
func isMounted(path string) bool {
	// Use mount command to check if path is mounted
	cmd := exec.Command("mount")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	// Check if the path appears in mount output
	return strings.Contains(string(output), path)
}
