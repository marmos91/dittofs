//go:build !windows

package share

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func validateUnmountPoint(mountPoint string) error {
	info, err := os.Stat(mountPoint)
	if os.IsNotExist(err) {
		return fmt.Errorf("mount point does not exist: %s\nHint: Check the path is correct", mountPoint)
	}
	if err != nil {
		return fmt.Errorf("failed to access mount point: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("mount point is not a directory: %s", mountPoint)
	}
	return nil
}

func checkUnmountPrivileges(mountPoint string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("unmount requires root privileges\nHint: Run with sudo: sudo dfsctl share unmount %s", mountPoint)
	}
	return nil
}

func isMountPoint(path string) bool {
	cmd := exec.Command("mount")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	normalizedPath := strings.TrimSuffix(path, "/")

	// Resolve symlinks for comparison since mount output shows resolved paths.
	// Common cases: macOS /tmp -> /private/tmp, Linux /var/run -> /run.
	pathsToCheck := []string{normalizedPath}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		if resolved := strings.TrimSuffix(resolved, "/"); resolved != normalizedPath {
			pathsToCheck = append(pathsToCheck, resolved)
		}
	}

	for _, line := range strings.Split(string(output), "\n") {
		for _, checkPath := range pathsToCheck {
			if strings.Contains(line, " on "+checkPath+" ") ||
				strings.Contains(line, " on "+checkPath+"\t") ||
				strings.HasSuffix(line, " on "+checkPath) {
				return true
			}
		}
	}

	return false
}

func performUnmount(mountPoint string, force bool) error {
	var cmd *exec.Cmd

	if runtime.GOOS == "darwin" {
		args := []string{"unmount"}
		if force {
			args = append(args, "force")
		}
		args = append(args, mountPoint)
		cmd = exec.Command("diskutil", args...)
	} else {
		args := []string{}
		if force {
			args = append(args, "-f")
		}
		args = append(args, mountPoint)
		cmd = exec.Command("umount", args...)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return formatUnmountError(err, string(output), force)
	}

	return nil
}
