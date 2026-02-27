//go:build !windows

package share

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

const (
	// Default file/dir modes: 0777 on macOS (can't set owner), 0755 on Linux (uid/gid works)
	defaultModeDarwin = "0777"
	defaultModeLinux  = "0755"
)

func getDefaultModeForPlatform() (string, string) {
	if runtime.GOOS == "darwin" {
		return defaultModeDarwin, "File permissions for SMB mount (octal, default 0777 on macOS since uid/gid not supported)"
	}
	return defaultModeLinux, "File permissions for SMB mount (octal)"
}

func validatePlatform() error {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return fmt.Errorf("unsupported platform: %s\nHint: Supported platforms are macOS, Linux, and Windows", runtime.GOOS)
	}
	return nil
}

func checkMountPrivileges(mountPoint, protocol, sharePath string) error {
	if os.Geteuid() == 0 {
		return nil
	}

	if runtime.GOOS == "darwin" {
		// On macOS, mounting to user's home directory works without sudo
		if homeDir, _ := os.UserHomeDir(); homeDir != "" && strings.HasPrefix(mountPoint, homeDir) {
			return nil
		}
	}

	// Check if mount point is owned by current user (may work without sudo on some systems)
	u, _ := user.Current()
	if u != nil {
		info, err := os.Stat(mountPoint)
		if err == nil {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				if strconv.FormatUint(uint64(stat.Uid), 10) == u.Uid {
					return nil
				}
			}
		}
	}

	fmt.Fprintf(os.Stderr, "Warning: mount commands typically require root privileges.\nTry: sudo dfsctl share mount --protocol %s %s %s\n", protocol, sharePath, mountPoint)
	return nil
}

func validateMountPoint(mountPoint string) error {
	info, err := os.Stat(mountPoint)
	if os.IsNotExist(err) {
		return fmt.Errorf("mount point does not exist: %s\nHint: Create the directory first with 'mkdir -p %s'", mountPoint, mountPoint)
	}
	if err != nil {
		return fmt.Errorf("failed to access mount point: %w", err)
	}

	if !info.IsDir() {
		return fmt.Errorf("mount point is not a directory: %s\nHint: Specify a directory path as the mount point", mountPoint)
	}

	entries, err := os.ReadDir(mountPoint)
	if err != nil {
		return fmt.Errorf("failed to read mount point directory: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("mount point is not empty: %s\nHint: Use an empty directory as the mount point", mountPoint)
	}

	return nil
}

func mountNFS(sharePath, mountPoint string, adapters []apiclient.Adapter, serverHost string) error {
	port := getAdapterPort(adapters, "nfs", defaultNFSPort)

	// actimeo=0 disables attribute caching for immediate visibility of changes
	mountOptions := fmt.Sprintf("nfsvers=3,tcp,port=%d,mountport=%d,actimeo=0", port, port)

	if runtime.GOOS == "darwin" {
		mountOptions += ",resvport"
	} else {
		mountOptions += ",nolock"
	}

	source := fmt.Sprintf("%s:%s", serverHost, sharePath)
	cmd := exec.Command("mount", "-t", "nfs", "-o", mountOptions, source, mountPoint)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return formatMountError(err, string(output), "NFS", port)
	}

	fmt.Printf("Mounted %s at %s (NFS)\n", sharePath, mountPoint)
	return nil
}

func mountSMB(sharePath, mountPoint string, adapters []apiclient.Adapter, serverHost string) error {
	port := getAdapterPort(adapters, "smb", defaultSMBPort)

	username, err := resolveSMBUsername()
	if err != nil {
		return err
	}

	password, err := resolveSMBPassword(username)
	if err != nil {
		return err
	}

	if runtime.GOOS == "darwin" {
		return mountSMBDarwin(sharePath, mountPoint, port, username, password, serverHost)
	}
	return mountSMBLinux(sharePath, mountPoint, port, username, password, serverHost)
}

func mountSMBLinux(sharePath, mountPoint string, port int, username, password, serverHost string) error {
	opts := fmt.Sprintf("vers=2.1,port=%d,username=%s,password=%s", port, username, password)

	// If running as root with SUDO_UID, set uid/gid so files are owned by original user
	if os.Geteuid() == 0 {
		if sudoUID := os.Getenv("SUDO_UID"); sudoUID != "" {
			opts += fmt.Sprintf(",uid=%s", sudoUID)
		}
		if sudoGID := os.Getenv("SUDO_GID"); sudoGID != "" {
			opts += fmt.Sprintf(",gid=%s", sudoGID)
		}
	}

	if mountFileMode != "" {
		opts += fmt.Sprintf(",file_mode=%s", mountFileMode)
	}
	if mountDirMode != "" {
		opts += fmt.Sprintf(",dir_mode=%s", mountDirMode)
	}

	uncPath := fmt.Sprintf("//%s%s", serverHost, sharePath)
	cmd := exec.Command("mount", "-t", "cifs", "-o", opts, uncPath, mountPoint)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return formatMountError(err, string(output), "SMB", port)
	}
	fmt.Printf("Mounted %s at %s via SMB (port %d)\n", sharePath, mountPoint, port)
	return nil
}

func mountSMBDarwin(sharePath, mountPoint string, port int, username, password, serverHost string) error {
	// On macOS, use mount_smbfs with file/dir mode flags.
	// If running with sudo, use sudo -u to mount as original user
	// (macOS security restriction: only mount owner can access files).
	smbURL := fmt.Sprintf("smb://%s:%s@%s:%d%s", username, password, serverHost, port, sharePath)

	args := []string{}
	if mountFileMode != "" {
		args = append(args, "-f", mountFileMode)
	}
	if mountDirMode != "" {
		args = append(args, "-d", mountDirMode)
	}
	args = append(args, smbURL, mountPoint)

	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
			sudoArgs := append([]string{"-u", sudoUser, "mount_smbfs"}, args...)
			cmd = exec.Command("sudo", sudoArgs...)
		} else {
			cmd = exec.Command("mount_smbfs", args...)
		}
	} else {
		cmd = exec.Command("mount_smbfs", args...)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Sanitize password from error output
		sanitized := strings.ReplaceAll(string(output), password, "****")
		return formatMountError(err, sanitized, "SMB", port)
	}
	fmt.Printf("Mounted %s at %s via SMB (port %d)\n", sharePath, mountPoint, port)
	return nil
}
