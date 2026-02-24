//go:build windows

package share

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func validateUnmountPoint(mountPoint string) error {
	// Drive letters (e.g. Z:) may fail os.Stat when the remote server is
	// unreachable, but they are still valid unmount targets. Skip the stat
	// check for drive letters and let isMountPoint handle validation.
	if len(mountPoint) == 2 && mountPoint[1] == ':' {
		return nil
	}

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
	// net use /delete does not require elevated privileges
	return nil
}

func isMountPoint(path string) bool {
	cmd := exec.Command("net", "use")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	normalizedPath := strings.ToUpper(strings.TrimSuffix(path, "\\"))

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		for _, field := range fields {
			if strings.ToUpper(field) == normalizedPath {
				return true
			}
		}
	}

	return false
}

func performUnmount(mountPoint string, force bool) error {
	args := []string{"use", mountPoint, "/delete"}
	if force {
		args = append(args, "/y")
	}

	cmd := exec.Command("net", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return formatUnmountError(err, string(output), force)
	}

	return nil
}
