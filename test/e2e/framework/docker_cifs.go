//go:build e2e

package framework

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SMBDockerMount represents an SMB share mounted via Docker container.
// This is used as a fallback when native CIFS mount is unavailable.
type SMBDockerMount struct {
	T           *testing.T
	ContainerID string // Docker container ID
	HostPath    string // Path on host where mount data is exchanged
	Protocol    string // Always "smb"
	Port        int    // SMB server port
	mounted     bool
}

// IsDockerAvailable checks if Docker is running and accessible.
func IsDockerAvailable() bool {
	cmd := exec.Command("docker", "version")
	err := cmd.Run()
	return err == nil
}

// IsNativeSMBAvailable checks if native CIFS/SMB mount is available.
// On Linux, checks for mount.cifs. On macOS, checks for mount_smbfs.
func IsNativeSMBAvailable() bool {
	switch {
	case fileExists("/sbin/mount.cifs"):
		return true
	case fileExists("/usr/sbin/mount.cifs"):
		return true
	case fileExists("/sbin/mount_smbfs"):
		return true
	case fileExists("/usr/sbin/mount_smbfs"):
		return true
	}
	// Try running mount -t cifs to see if kernel module is available
	cmd := exec.Command("mount", "-t", "cifs")
	output, _ := cmd.CombinedOutput()
	// If it says "unknown filesystem type" it's not available
	return !strings.Contains(string(output), "unknown filesystem type")
}

// SkipIfNoSMBMount skips the test if no SMB mount capability is available.
func SkipIfNoSMBMount(t *testing.T) {
	t.Helper()
	if !IsNativeSMBAvailable() && !IsDockerAvailable() {
		t.Skip("Skipping: requires SMB mount capability (CIFS client or Docker)")
	}
}

// MountSMBWithDocker mounts an SMB share using a Docker container with CIFS utils.
// This is a fallback for systems without native CIFS mount capability.
//
// The approach:
// 1. Create a temp directory on the host for file exchange
// 2. Run an Alpine container with cifs-utils that mounts the SMB share
// 3. Files can be accessed via docker cp or docker exec
//
// Note: This requires Docker to be installed and the user to have docker permissions.
// The container runs in privileged mode to allow mount operations.
func MountSMBWithDocker(t *testing.T, port int, creds SMBCredentials) *SMBDockerMount {
	t.Helper()

	if !IsDockerAvailable() {
		t.Fatal("Docker is not available for SMB mount fallback")
	}

	// Create unique ID for this mount
	mountID := fmt.Sprintf("smb-%d-%d", port, time.Now().UnixNano())

	// Create host directory for file exchange
	hostPath, err := os.MkdirTemp("", "dittofs-docker-smb-*")
	if err != nil {
		t.Fatalf("Failed to create temp directory for Docker SMB mount: %v", err)
	}

	// Build the mount command that will run inside the container
	// Using Alpine with cifs-utils for minimal image size
	mountCmd := fmt.Sprintf(
		"apk add --no-cache cifs-utils > /dev/null 2>&1 && "+
			"mkdir -p /mnt/smb && "+
			"mount -t cifs //host.docker.internal/export /mnt/smb "+
			"-o port=%d,username=%s,password=%s,vers=2.1,sec=ntlmssp && "+
			"tail -f /dev/null",
		port, creds.Username, creds.Password,
	)

	// Run Docker container
	// --privileged is required for mount operations inside container
	// --add-host adds host.docker.internal for Docker Desktop compatibility
	args := []string{
		"run", "-d", "--rm",
		"--privileged",
		"--name", mountID,
		"--add-host", "host.docker.internal:host-gateway",
		"-v", fmt.Sprintf("%s:/data", hostPath),
		"alpine:latest",
		"sh", "-c", mountCmd,
	}

	cmd := exec.Command("docker", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(hostPath)
		t.Fatalf("Failed to start Docker container for SMB mount: %v\nOutput: %s\nCommand: docker %v",
			err, string(output), args)
	}

	containerID := strings.TrimSpace(string(output))
	t.Logf("Started Docker container %s for SMB mount", containerID[:12])

	// Wait for mount to be ready
	if err := waitForDockerMount(t, containerID, 30*time.Second); err != nil {
		// Cleanup on failure
		_ = exec.Command("docker", "stop", containerID).Run()
		_ = os.RemoveAll(hostPath)
		t.Fatalf("Docker SMB mount failed to become ready: %v", err)
	}

	t.Logf("Docker SMB mount ready (container: %s)", containerID[:12])

	return &SMBDockerMount{
		T:           t,
		ContainerID: containerID,
		HostPath:    hostPath,
		Protocol:    "smb",
		Port:        port,
		mounted:     true,
	}
}

// waitForDockerMount waits for the SMB mount inside the container to be ready.
func waitForDockerMount(t *testing.T, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		// Check if mount point exists and is accessible
		cmd := exec.Command("docker", "exec", containerID, "ls", "/mnt/smb")
		if err := cmd.Run(); err == nil {
			return nil
		}

		// Check container logs for errors
		logCmd := exec.Command("docker", "logs", "--tail", "20", containerID)
		logs, _ := logCmd.CombinedOutput()

		// Check for common errors
		logStr := string(logs)
		if strings.Contains(logStr, "mount error") ||
			strings.Contains(logStr, "Connection refused") ||
			strings.Contains(logStr, "No route to host") {
			return fmt.Errorf("mount failed: %s", logStr)
		}

		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for Docker SMB mount")
}

// Cleanup stops the Docker container and removes the host directory.
func (m *SMBDockerMount) Cleanup() {
	if !m.mounted {
		return
	}

	m.T.Helper()

	// Stop the container (--rm flag ensures it's removed)
	cmd := exec.Command("docker", "stop", m.ContainerID)
	if output, err := cmd.CombinedOutput(); err != nil {
		m.T.Logf("Warning: failed to stop Docker container %s: %v\nOutput: %s",
			m.ContainerID[:12], err, string(output))
	} else {
		m.T.Logf("Stopped Docker SMB mount container %s", m.ContainerID[:12])
	}

	// Remove host directory
	if m.HostPath != "" {
		_ = os.RemoveAll(m.HostPath)
	}

	m.mounted = false
}

// FilePath returns the path for a file within the Docker-mounted SMB share.
// For Docker mounts, this returns the container-internal path that should be
// used with docker exec or docker cp commands.
func (m *SMBDockerMount) FilePath(relativePath string) string {
	return filepath.Join("/mnt/smb", relativePath)
}

// WriteFile writes content to a file in the Docker-mounted SMB share.
func (m *SMBDockerMount) WriteFile(content []byte, relativePath string) error {
	// Write to host path first, then copy into container
	hostFile := filepath.Join(m.HostPath, "temp_write")
	if err := os.WriteFile(hostFile, content, 0644); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	defer os.Remove(hostFile)

	// Copy into container's mounted SMB share
	containerPath := m.FilePath(relativePath)
	cmd := exec.Command("docker", "exec", m.ContainerID, "sh", "-c",
		fmt.Sprintf("cat > '%s'", containerPath))
	cmd.Stdin = bytes.NewReader(content)

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to write file in container: %v\nOutput: %s", err, string(output))
	}

	return nil
}

// ReadFile reads content from a file in the Docker-mounted SMB share.
func (m *SMBDockerMount) ReadFile(relativePath string) ([]byte, error) {
	containerPath := m.FilePath(relativePath)
	cmd := exec.Command("docker", "exec", m.ContainerID, "cat", containerPath)

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to read file from container: %w", err)
	}

	return output, nil
}

// Exec runs a command inside the Docker container.
func (m *SMBDockerMount) Exec(command string) ([]byte, error) {
	cmd := exec.Command("docker", "exec", m.ContainerID, "sh", "-c", command)
	return cmd.CombinedOutput()
}

// fileExists checks if a file exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
