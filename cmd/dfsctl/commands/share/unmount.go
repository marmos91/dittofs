package share

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var unmountForce bool

var unmountCmd = &cobra.Command{
	Use:   "unmount [mountpoint]",
	Short: "Unmount a mounted share",
	Long: `Unmount a DittoFS share from a local mount point.

Examples:
  # Unmount a share
  dfsctl share unmount /mnt/dittofs

  # Force unmount if busy
  dfsctl share unmount --force /mnt/dittofs

  # Windows: unmount a mapped drive
  dfsctl share unmount Z:

Note: Unmount commands typically require sudo/root privileges on Unix systems.`,
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

	if err := validateUnmountPoint(mountPoint); err != nil {
		return err
	}

	if !isMountPoint(mountPoint) {
		return fmt.Errorf("path is not a mount point: %s\nHint: The path does not appear to be a mounted filesystem", mountPoint)
	}

	if err := checkUnmountPrivileges(mountPoint); err != nil {
		return err
	}

	if err := performUnmount(mountPoint, unmountForce); err != nil {
		return err
	}

	fmt.Printf("Unmounted %s\n", mountPoint)
	return nil
}

func formatUnmountError(err error, output string, forceAttempted bool) error {
	combined := strings.ToLower(output + " " + err.Error())

	if strings.Contains(combined, "not currently mounted") ||
		strings.Contains(combined, "not mounted") ||
		strings.Contains(combined, "no mount point") {
		return fmt.Errorf("unmount failed: %w\nOutput: %s\nHint: The path does not appear to be a mount point", err, output)
	}

	if strings.Contains(combined, "busy") {
		if forceAttempted {
			return fmt.Errorf("unmount failed: %w\nOutput: %s\nHint: Some process is using the mount. Check with 'lsof +D %s'", err, output, output)
		}
		return fmt.Errorf("unmount failed: %w\nOutput: %s\nHint: Files may be in use. Close applications and try again, or use --force", err, output)
	}

	if strings.Contains(combined, "permission denied") || strings.Contains(combined, "operation not permitted") {
		return fmt.Errorf("unmount failed: %w\nOutput: %s\nHint: Unmount may require sudo privileges", err, output)
	}

	return fmt.Errorf("unmount failed: %w\nOutput: %s", err, output)
}
