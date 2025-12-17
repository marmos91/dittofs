//go:build e2e

package framework

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Mount represents a mounted filesystem (NFS or SMB).
type Mount struct {
	T        *testing.T
	Path     string
	Protocol string // "nfs" or "smb"
	Port     int
	mounted  bool
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
	mountOptions := fmt.Sprintf("nfsvers=3,tcp,port=%d,mountport=%d", port, port)
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
func MountSMB(t *testing.T, port int, creds SMBCredentials) *Mount {
	t.Helper()

	// Give the SMB server a moment to fully initialize
	time.Sleep(500 * time.Millisecond)

	// Create mount directory
	mountPath, err := os.MkdirTemp("", "dittofs-e2e-smb-*")
	if err != nil {
		t.Fatalf("Failed to create SMB mount directory: %v", err)
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
			"-o", fmt.Sprintf("port=%d,username=%s,password=%s,vers=2.0",
				port, creds.Username, creds.Password))
	default:
		_ = os.RemoveAll(mountPath)
		t.Fatalf("Unsupported platform for SMB: %s", runtime.GOOS)
	}

	// Execute mount command with retries
	var output []byte
	var lastErr error
	maxRetries := 3

	for i := 0; i < maxRetries; i++ {
		output, lastErr = cmd.CombinedOutput()

		if lastErr == nil {
			t.Logf("SMB share mounted successfully at %s", mountPath)
			break
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
					"-o", fmt.Sprintf("port=%d,username=%s,password=%s,vers=2.0",
						port, creds.Username, creds.Password))
			}
		}
	}

	if lastErr != nil {
		_ = os.RemoveAll(mountPath)
		t.Fatalf("Failed to mount SMB share after %d attempts: %v\nOutput: %s",
			maxRetries, lastErr, string(output))
	}

	return &Mount{
		T:        t,
		Path:     mountPath,
		Protocol: "smb",
		Port:     port,
		mounted:  true,
	}
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

	// Wait a moment for macOS to release SMB session state
	if runtime.GOOS == "darwin" && m.Protocol == "smb" {
		time.Sleep(200 * time.Millisecond)
	}

	m.mounted = false
}

// Cleanup unmounts and removes the mount directory.
func (m *Mount) Cleanup() {
	m.T.Helper()
	m.Unmount()
	if m.Path != "" {
		_ = os.RemoveAll(m.Path)
	}
}

// FilePath returns the absolute path for a relative path within the mount.
func (m *Mount) FilePath(relativePath string) string {
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
