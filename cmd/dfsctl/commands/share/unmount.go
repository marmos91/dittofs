package share

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

var (
	unmountForce bool
)

var unmountCmd = &cobra.Command{
	Use:   "unmount [mountpoint]",
	Short: "Unmount a mounted share",
	Long: `Unmount a DittoFS share from a local mount point.

Examples:
  # Unmount a share
  dfsctl share unmount /mnt/dittofs

  # Force unmount if busy
  dfsctl share unmount --force /mnt/dittofs

Note: Unmount commands typically require sudo/root privileges.`,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) < 1 {
			return fmt.Errorf("requires mount point path\n\nUsage: dfsctl share unmount [mountpoint]\n\nExample: dfsctl share unmount /mnt/dittofs")
		}
		if len(args) > 1 {
			return fmt.Errorf("accepts 1 arg, received %d", len(args))
		}
		return nil
	},
	RunE: runUnmount,
}

func init() {
	unmountCmd.Flags().BoolVarP(&unmountForce, "force", "f", false, "Force unmount even if busy")
}

func runUnmount(cmd *cobra.Command, args []string) error {
	mountPoint := args[0]

	// Validate platform
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return fmt.Errorf("unsupported platform: %s\nHint: Supported platforms are macOS and Linux", runtime.GOOS)
	}

	// Validate mount point exists
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

	// Check if it's actually mounted
	if !isMountPoint(mountPoint) {
		return fmt.Errorf("path is not a mount point: %s\nHint: The path does not appear to be a mounted filesystem", mountPoint)
	}

	// Check for root privileges (required for unmount)
	if os.Geteuid() != 0 {
		return fmt.Errorf("unmount requires root privileges\nHint: Run with sudo: sudo dfsctl share unmount %s", mountPoint)
	}

	// Perform unmount
	if err := performUnmount(mountPoint, unmountForce); err != nil {
		return err
	}

	fmt.Printf("Unmounted %s\n", mountPoint)
	return nil
}

func isMountPoint(path string) bool {
	// Use mount command to check if path is mounted
	cmd := exec.Command("mount")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	// Normalize the path for comparison
	normalizedPath := strings.TrimSuffix(path, "/")

	// Also get the real path (resolve symlinks) for comparison.
	// Common symlink scenarios across platforms:
	// - macOS: /tmp -> /private/tmp, /var -> /private/var
	// - Linux: /var/run -> /run on some distributions
	// Mount output shows the resolved path, so we check both.
	realPath := normalizedPath
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		realPath = strings.TrimSuffix(resolved, "/")
	}

	// Check if either path appears in mount output
	// Mount output format varies by OS:
	// macOS: /dev/disk1 on /path (type)
	// Linux: /dev/sda1 on /path type ext4 (options)
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		// Look for " on /path " or " on /path\t" patterns
		// Check both the original path and the resolved real path
		for _, checkPath := range []string{normalizedPath, realPath} {
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

	switch runtime.GOOS {
	case "darwin":
		if force {
			cmd = exec.Command("diskutil", "unmount", "force", mountPoint)
		} else {
			cmd = exec.Command("diskutil", "unmount", mountPoint)
		}
	case "linux":
		if force {
			cmd = exec.Command("umount", "-f", mountPoint)
		} else {
			cmd = exec.Command("umount", mountPoint)
		}
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return formatUnmountError(err, string(output), force)
	}

	return nil
}

func formatUnmountError(err error, output string, forceAttempted bool) error {
	outputLower := strings.ToLower(output)
	errStr := strings.ToLower(err.Error())

	// Check for common error patterns
	if strings.Contains(outputLower, "not currently mounted") ||
		strings.Contains(outputLower, "not mounted") ||
		strings.Contains(outputLower, "no mount point") {
		return fmt.Errorf("unmount failed: %w\nOutput: %s\nHint: The path does not appear to be a mount point", err, output)
	}

	if strings.Contains(outputLower, "busy") || strings.Contains(outputLower, "resource busy") ||
		strings.Contains(outputLower, "device is busy") || strings.Contains(errStr, "busy") {
		if forceAttempted {
			return fmt.Errorf("unmount failed: %w\nOutput: %s\nHint: Some process is using the mount. Check with 'lsof +D %s'", err, output, output)
		}
		return fmt.Errorf("unmount failed: %w\nOutput: %s\nHint: Files may be in use. Close applications and try again, or use --force", err, output)
	}

	if strings.Contains(outputLower, "permission denied") || strings.Contains(outputLower, "operation not permitted") ||
		strings.Contains(errStr, "permission denied") {
		return fmt.Errorf("unmount failed: %w\nOutput: %s\nHint: Unmount may require sudo privileges", err, output)
	}

	// Generic error
	return fmt.Errorf("unmount failed: %w\nOutput: %s", err, output)
}
