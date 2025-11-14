package framework

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// MountConfig holds configuration for mounting NFS
type MountConfig struct {
	ServerAddr string
	ServerPort int
	ExportPath string
	MountPoint string
	NFSVersion string // "3" for NFSv3
	Options    []string
}

// NFSMount represents a mounted NFS filesystem
type NFSMount struct {
	t          testing.TB
	config     MountConfig
	mountPoint string
	mounted    bool
}

// NewNFSMount creates a new NFS mount helper
func NewNFSMount(t testing.TB, config MountConfig) *NFSMount {
	t.Helper()

	// Set defaults
	if config.ServerAddr == "" {
		config.ServerAddr = "localhost"
	}
	if config.NFSVersion == "" {
		config.NFSVersion = "3"
	}

	// Create mount point if not specified
	if config.MountPoint == "" {
		mountPoint, err := os.MkdirTemp("", "dittofs-mount-*")
		if err != nil {
			t.Fatalf("Failed to create mount point: %v", err)
		}
		config.MountPoint = mountPoint
	} else {
		// Ensure mount point exists
		if err := os.MkdirAll(config.MountPoint, 0755); err != nil {
			t.Fatalf("Failed to create mount point directory: %v", err)
		}
	}

	return &NFSMount{
		t:          t,
		config:     config,
		mountPoint: config.MountPoint,
	}
}

// Mount performs the NFS mount operation
func (m *NFSMount) Mount() error {
	m.t.Helper()

	if m.mounted {
		return fmt.Errorf("already mounted")
	}

	// Build mount command based on OS
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = m.buildDarwinMountCmd()
	case "linux":
		cmd = m.buildLinuxMountCmd()
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	m.t.Logf("Mounting NFS: %s", cmd.String())

	// Execute mount command with timeout to prevent hanging
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Set up command context for timeout
	cmd.WaitDelay = 1 * time.Second

	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("mount command timed out after 30 seconds")
	}
	if err != nil {
		// Provide helpful error message
		errMsg := fmt.Sprintf("mount failed: %v\nOutput: %s", err, string(output))
		if runtime.GOOS == "darwin" && strings.Contains(string(output), "Operation not permitted") {
			errMsg += "\n\nNote: On macOS, NFS mounting typically requires sudo privileges."
			errMsg += "\nYou may need to run tests with sudo or configure your system to allow non-root NFS mounts."
		}
		return fmt.Errorf("%s", errMsg)
	}

	// Verify mount succeeded
	if err := m.verifyMount(); err != nil {
		return fmt.Errorf("mount verification failed: %w", err)
	}

	m.mounted = true
	m.t.Logf("Successfully mounted at %s", m.mountPoint)
	return nil
}

// Unmount unmounts the NFS filesystem
func (m *NFSMount) Unmount() error {
	m.t.Helper()

	if !m.mounted {
		return nil
	}

	m.t.Logf("Unmounting %s", m.mountPoint)

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin", "linux":
		cmd = exec.Command("umount", m.mountPoint)
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Try force unmount as fallback
		m.t.Logf("Normal unmount failed, trying force unmount: %s", string(output))
		return m.forceUnmount()
	}

	// Clean up mount point
	if err := os.Remove(m.mountPoint); err != nil {
		m.t.Logf("Warning: failed to remove mount point: %v", err)
	}

	m.mounted = false
	m.t.Logf("Successfully unmounted")
	return nil
}

// MountPoint returns the local mount point path
func (m *NFSMount) MountPoint() string {
	return m.mountPoint
}

// buildDarwinMountCmd builds the mount command for macOS
func (m *NFSMount) buildDarwinMountCmd() *exec.Cmd {
	// macOS NFS mount format:
	// mount -t nfs -o nfsvers=3,tcp,port=2049 localhost:/export /mnt/test

	opts := []string{
		fmt.Sprintf("nfsvers=%s", m.config.NFSVersion),
		"tcp",
		fmt.Sprintf("port=%d", m.config.ServerPort),
		fmt.Sprintf("mountport=%d", m.config.ServerPort), // Mount protocol on same port
		"nolocks",   // Disable file locking (NLM not implemented)
		"hard",      // Hard mount (recommended for testing)
		"intr",      // Allow interruption
		"timeo=10",  // Shorter timeout for faster failures
		"retrans=2", // Fewer retransmissions
	}

	// Add custom options
	opts = append(opts, m.config.Options...)

	nfsSource := fmt.Sprintf("%s:%s", m.config.ServerAddr, m.config.ExportPath)

	return exec.Command("mount",
		"-t", "nfs",
		"-o", strings.Join(opts, ","),
		nfsSource,
		m.mountPoint,
	)
}

// buildLinuxMountCmd builds the mount command for Linux
func (m *NFSMount) buildLinuxMountCmd() *exec.Cmd {
	// Linux NFS mount format:
	// mount -t nfs -o nfsvers=3,tcp,port=2049 localhost:/export /mnt/test

	opts := []string{
		fmt.Sprintf("nfsvers=%s", m.config.NFSVersion),
		"tcp",
		fmt.Sprintf("port=%d", m.config.ServerPort),
		fmt.Sprintf("mountport=%d", m.config.ServerPort), // Mount protocol on same port
		"nolock", // Disable file locking (NLM not implemented)
	}

	// Add custom options
	opts = append(opts, m.config.Options...)

	nfsSource := fmt.Sprintf("%s:%s", m.config.ServerAddr, m.config.ExportPath)

	return exec.Command("mount",
		"-t", "nfs",
		"-o", strings.Join(opts, ","),
		nfsSource,
		m.mountPoint,
	)
}

// verifyMount verifies the mount was successful
func (m *NFSMount) verifyMount() error {
	// Try to access the mount point
	entries, err := os.ReadDir(m.mountPoint)
	if err != nil {
		return fmt.Errorf("failed to read mount point: %w", err)
	}

	m.t.Logf("Mount point readable, contains %d entries", len(entries))

	// On some systems, check if it shows up in mount table
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		cmd := exec.Command("mount")
		output, err := cmd.Output()
		if err == nil {
			if strings.Contains(string(output), m.mountPoint) {
				m.t.Logf("Mount verified in mount table")
				return nil
			}
		}
	}

	return nil
}

// forceUnmount attempts a force unmount
func (m *NFSMount) forceUnmount() error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("umount", "-f", m.mountPoint)
	case "linux":
		cmd = exec.Command("umount", "-l", m.mountPoint) // lazy unmount
	default:
		return fmt.Errorf("force unmount not supported on %s", runtime.GOOS)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("force unmount failed: %w\nOutput: %s", err, string(output))
	}

	// Clean up mount point
	time.Sleep(100 * time.Millisecond) // Give time for lazy unmount
	if err := os.Remove(m.mountPoint); err != nil {
		m.t.Logf("Warning: failed to remove mount point: %v", err)
	}

	m.mounted = false
	return nil
}

// Cleanup ensures the mount is unmounted and cleaned up
func (m *NFSMount) Cleanup() {
	m.t.Helper()
	if err := m.Unmount(); err != nil {
		m.t.Logf("Cleanup unmount error: %v", err)
		// Try force unmount as last resort
		if err := m.forceUnmount(); err != nil {
			m.t.Errorf("Failed to cleanup mount: %v", err)
		}
	}
}

// IsCommandAvailable checks if a command is available in PATH
func IsCommandAvailable(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

// CanMount checks if NFS mounting is available on this system
func CanMount(t testing.TB) bool {
	t.Helper()

	// Check if mount command exists
	if !IsCommandAvailable("mount") {
		t.Log("mount command not available")
		return false
	}

	// Check if umount command exists
	if !IsCommandAvailable("umount") {
		t.Log("umount command not available")
		return false
	}

	// Platform-specific checks
	switch runtime.GOOS {
	case "darwin", "linux":
		// Check if NFS kernel module is available (Linux)
		if runtime.GOOS == "linux" {
			if _, err := os.Stat("/proc/filesystems"); err == nil {
				data, _ := os.ReadFile("/proc/filesystems")
				if !strings.Contains(string(data), "nfs") {
					t.Log("NFS not available in /proc/filesystems")
					return false
				}
			}
		}
		return true
	default:
		t.Logf("Unsupported platform: %s", runtime.GOOS)
		return false
	}
}

// CreateTestFile creates a test file with specified content
func CreateTestFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("Failed to create parent directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
}

// VerifyFileContent verifies a file has expected content
func VerifyFileContent(t *testing.T, path string, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}
	if string(data) != expected {
		t.Fatalf("File content mismatch:\nExpected: %q\nGot: %q", expected, string(data))
	}
}
