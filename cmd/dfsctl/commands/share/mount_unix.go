//go:build !windows

package share

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

const (
	// Default file/dir modes: 0777 on macOS (can't set owner), 0755 on Linux (uid/gid works)
	defaultModeDarwin = "0777"
	defaultModeLinux  = "0755"
)

func getDefaultModeForPlatform() (mode, help string) {
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

		return fmt.Errorf(
			"mount requires root privileges (or mount to home directory on macOS)\nHint: Run with sudo: sudo dfsctl share mount --protocol %s %s %s\n"+
				"Or mount to your home directory: dfsctl share mount --protocol %s %s ~/mnt/share",
			protocol, sharePath, mountPoint, protocol, sharePath,
		)
	}

	return fmt.Errorf(
		"mount requires root privileges\nHint: Run with sudo: sudo dfsctl share mount --protocol %s %s %s",
		protocol, sharePath, mountPoint,
	)
}

func validateMountPoint(mountPoint string) error {
	info, err := os.Stat(mountPoint)
	if os.IsNotExist(err) {
		return fmt.Errorf("mount point does not exist: %s\nHint: Create the directory first with 'mkdir %s'", mountPoint, mountPoint)
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

func mountNFS(sharePath, mountPoint string, adapters []apiclient.Adapter) error {
	port := getAdapterPort(adapters, "nfs", defaultNFSPort)

	// actimeo=0 disables attribute caching for immediate visibility of changes
	mountOptions := fmt.Sprintf("nfsvers=3,tcp,port=%d,mountport=%d,actimeo=0", port, port)

	if runtime.GOOS == "darwin" {
		mountOptions += ",resvport"
	} else {
		mountOptions += ",nolock"
	}

	source := fmt.Sprintf("localhost:%s", sharePath)
	cmd := exec.Command("mount", "-t", "nfs", "-o", mountOptions, source, mountPoint)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return formatMountError(err, string(output), "NFS", port)
	}

	fmt.Printf("Mounted %s at %s (NFS)\n", sharePath, mountPoint)
	return nil
}

func mountSMB(sharePath, mountPoint string, adapters []apiclient.Adapter) error {
	port := getAdapterPort(adapters, "smb", defaultSMBPort)

	username, err := resolveSMBUsername()
	if err != nil {
		return err
	}

	password, err := resolveSMBPassword(username)
	if err != nil {
		return err
	}

	shareName := strings.TrimPrefix(sharePath, "/")
	cmd := buildSMBMountCommand(username, password, port, shareName, mountPoint)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return formatMountError(err, string(output), "SMB", port)
	}

	fmt.Printf("Mounted %s at %s (SMB)\n", sharePath, mountPoint)
	return nil
}

func buildSMBMountCommand(username, password string, port int, shareName, mountPoint string) *exec.Cmd {
	if runtime.GOOS == "darwin" {
		return buildDarwinSMBCommand(username, password, port, shareName, mountPoint)
	}
	return buildLinuxSMBCommand(username, password, port, shareName, mountPoint)
}

func buildDarwinSMBCommand(username, password string, port int, shareName, mountPoint string) *exec.Cmd {
	// macOS: mount_smbfs -f MODE -d MODE //USER:PASS@localhost:PORT/SHARE MOUNTPOINT
	// Note: Modern macOS (Ventura+) removed -u/-g options for setting owner.
	// Files will be owned by root when mounted with sudo.
	// Default is 0777 to allow all users to write to the mounted share.
	smbURL := fmt.Sprintf("//%s:%s@localhost:%d/%s", username, password, port, shareName)
	args := []string{"-f", mountFileMode, "-d", mountDirMode, smbURL, mountPoint}

	// macOS security restriction: only the mount owner can access files,
	// regardless of Unix permissions (Apple's "works as intended" behavior).
	// When running with sudo, we use "sudo -u <original_user>" to mount as
	// that user instead of root, so they can actually access the mounted files.
	// See: https://discussions.apple.com/thread/4927134
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		return exec.Command("sudo", append([]string{"-u", sudoUser, "mount_smbfs"}, args...)...)
	}
	return exec.Command("mount_smbfs", args...)
}

func buildLinuxSMBCommand(username, password string, port int, shareName, mountPoint string) *exec.Cmd {
	// Linux: mount -t cifs //localhost/SHARE MOUNTPOINT -o port=PORT,username=USER,password=PASS,vers=2.1,uid=UID,gid=GID
	// When running with sudo, set uid/gid to the original user (not root)
	mountOpts := fmt.Sprintf("port=%d,username=%s,password=%s,vers=2.1", port, username, password)

	if sudoUID := os.Getenv("SUDO_UID"); sudoUID != "" {
		mountOpts += ",uid=" + sudoUID
	}
	if sudoGID := os.Getenv("SUDO_GID"); sudoGID != "" {
		mountOpts += ",gid=" + sudoGID
	}

	return exec.Command("mount", "-t", "cifs",
		fmt.Sprintf("//localhost/%s", shareName),
		mountPoint,
		"-o", mountOpts)
}
