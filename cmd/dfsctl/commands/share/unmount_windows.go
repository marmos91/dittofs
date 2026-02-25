//go:build windows

package share

import (
	"fmt"
	"os/exec"
	"strings"
)

func validateUnmountPoint(mountPoint string) error {
	if mountPoint == "" {
		return fmt.Errorf("mount point cannot be empty")
	}
	return nil
}

func isMountPoint(mountPoint string) bool {
	// On Windows, check if drive letter is in use via net use
	cmd := exec.Command("net", "use")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), strings.ToUpper(mountPoint))
}

func checkUnmountPrivileges(mountPoint string) error {
	return nil
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
