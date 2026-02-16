package share

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/credentials"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

const (
	defaultNFSPort = 12049
	defaultSMBPort = 12445

	// Default file/dir modes: 0777 on macOS (can't set owner), 0755 on Linux (uid/gid works)
	defaultModeDarwin = "0777"
	defaultModeLinux  = "0755"
)

var (
	mountProtocol string
	mountUsername string
	mountPassword string
	mountFileMode string
	mountDirMode  string
)

var mountCmd = &cobra.Command{
	Use:   "mount [share] [mountpoint]",
	Short: "Mount a share via NFS or SMB",
	Long: `Mount a DittoFS share at a local mount point using NFS or SMB protocol.

For SMB mounts, credentials are resolved in order:
  1. --username/--password flags
  2. DITTOFS_PASSWORD environment variable (for password)
  3. Current login context username
  4. Interactive password prompt

Examples:
  # Mount via NFS
  dfsctl share mount --protocol nfs /export /mnt/dittofs

  # Mount via SMB
  dfsctl share mount --protocol smb /export /mnt/dittofs

  # Mount via SMB with explicit credentials
  dfsctl share mount --protocol smb --username alice /export /mnt/dittofs

  # Mount via SMB with password from environment
  DITTOFS_PASSWORD=secret dfsctl share mount --protocol smb /export /mnt/dittofs

  # Mount to user directory without sudo (macOS only, recommended)
  mkdir -p ~/mnt/dittofs && dfsctl share mount --protocol smb /export ~/mnt/dittofs

Note: Mount commands typically require sudo/root privileges.

Platform differences for SMB with sudo:
  - Linux: Mount owner set to your user via uid/gid options (default mode 0755)
  - macOS: Mount owned by root (uid/gid removed in Catalina), default mode 0777
  - macOS alternative: mount to ~/mnt without sudo for user-owned mount`,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) < 2 {
			return fmt.Errorf("requires share path and mount point\n\nUsage: dfsctl share mount [share] [mountpoint] --protocol <nfs|smb>\n\nExample: dfsctl share mount --protocol nfs /export /mnt/dittofs")
		}
		if len(args) > 2 {
			return fmt.Errorf("accepts 2 args, received %d", len(args))
		}
		return nil
	},
	RunE: runMount,
}

func init() {
	defaultMode, modeHelp := getDefaultModeForPlatform()

	mountCmd.Flags().StringVarP(&mountProtocol, "protocol", "p", "", "Protocol to use (nfs or smb) (required)")
	mountCmd.Flags().StringVarP(&mountUsername, "username", "u", "", "Username for SMB mount (defaults to login username)")
	mountCmd.Flags().StringVarP(&mountPassword, "password", "P", "", "Password for SMB mount (will prompt if not provided)")
	mountCmd.Flags().StringVar(&mountFileMode, "file-mode", defaultMode, modeHelp)
	mountCmd.Flags().StringVar(&mountDirMode, "dir-mode", defaultMode, "Directory permissions for SMB mount (octal)")

	_ = mountCmd.MarkFlagRequired("protocol")
}

func getDefaultModeForPlatform() (mode, help string) {
	if runtime.GOOS == "darwin" {
		return defaultModeDarwin, "File permissions for SMB mount (octal, default 0777 on macOS since uid/gid not supported)"
	}
	return defaultModeLinux, "File permissions for SMB mount (octal)"
}

func runMount(cmd *cobra.Command, args []string) error {
	sharePath := args[0]
	mountPoint := args[1]

	// Validate protocol
	protocol := strings.ToLower(mountProtocol)
	if protocol != "nfs" && protocol != "smb" {
		return fmt.Errorf("invalid protocol '%s': must be 'nfs' or 'smb'\nHint: Use --protocol nfs or --protocol smb", mountProtocol)
	}

	if err := validatePlatform(); err != nil {
		return err
	}

	if err := validateMountPoint(mountPoint); err != nil {
		return err
	}

	if err := checkMountPrivileges(mountPoint, protocol, sharePath); err != nil {
		return err
	}

	// Get authenticated client to fetch adapter ports
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return fmt.Errorf("failed to get authenticated client: %w\nHint: Run 'dfsctl login' first", err)
	}

	// Fetch adapter info to get port
	adapters, err := client.ListAdapters()
	if err != nil {
		return fmt.Errorf("failed to list adapters: %w\nHint: Is the DittoFS server running?", err)
	}

	switch protocol {
	case "nfs":
		return mountNFS(sharePath, mountPoint, adapters)
	case "smb":
		return mountSMB(sharePath, mountPoint, adapters)
	default:
		return fmt.Errorf("unsupported protocol: %s", protocol)
	}
}

func validatePlatform() error {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return fmt.Errorf("unsupported platform: %s\nHint: Supported platforms are macOS and Linux", runtime.GOOS)
	}
	return nil
}

func checkMountPrivileges(mountPoint, protocol, sharePath string) error {
	if os.Geteuid() == 0 {
		return nil
	}

	// On macOS, mounting to user's home directory works without sudo
	if runtime.GOOS == "darwin" {
		if homeDir, _ := os.UserHomeDir(); homeDir != "" && strings.HasPrefix(mountPoint, homeDir) {
			return nil
		}
	}

	hint := fmt.Sprintf("Run with sudo: sudo dfsctl share mount --protocol %s %s %s", protocol, sharePath, mountPoint)
	if runtime.GOOS == "darwin" {
		hint += fmt.Sprintf("\nOr mount to your home directory: dfsctl share mount --protocol %s %s ~/mnt/share", protocol, sharePath)
	}
	return fmt.Errorf("mount requires root privileges (or mount to home directory on macOS)\nHint: %s", hint)
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

	// Check if directory is empty
	entries, err := os.ReadDir(mountPoint)
	if err != nil {
		return fmt.Errorf("failed to read mount point directory: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("mount point is not empty: %s\nHint: Use an empty directory as the mount point", mountPoint)
	}

	return nil
}

func getAdapterPort(adapters []apiclient.Adapter, protocol string, defaultPort int) int {
	for _, adapter := range adapters {
		if strings.EqualFold(adapter.Type, protocol) && adapter.Enabled {
			return adapter.Port
		}
	}
	return defaultPort
}

func mountNFS(sharePath, mountPoint string, adapters []apiclient.Adapter) error {
	port := getAdapterPort(adapters, "nfs", defaultNFSPort)

	// actimeo=0 disables attribute caching for immediate visibility of changes
	mountOptions := fmt.Sprintf("nfsvers=3,tcp,port=%d,mountport=%d,actimeo=0", port, port)

	// Add platform-specific options
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

func resolveSMBUsername() (string, error) {
	if mountUsername != "" {
		return mountUsername, nil
	}

	// Try to get username from current login context
	store, err := credentials.NewStore()
	if err == nil {
		if ctx, err := store.GetCurrentContext(); err == nil && ctx.Username != "" {
			return ctx.Username, nil
		}
	}

	return "", fmt.Errorf("username required for SMB mount\nHint: Use --username flag or login with 'dfsctl login'")
}

func resolveSMBPassword(username string) (string, error) {
	// Priority: flag > environment > prompt
	if mountPassword != "" {
		return mountPassword, nil
	}

	if envPassword := os.Getenv("DITTOFS_PASSWORD"); envPassword != "" {
		return envPassword, nil
	}

	password, err := prompt.Password(fmt.Sprintf("Password for %s", username))
	if err != nil {
		return "", cmdutil.HandleAbort(err)
	}
	return password, nil
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

func formatMountError(err error, output, protocol string, port int) error {
	outputLower := strings.ToLower(output)
	errStr := strings.ToLower(err.Error())

	// Check for common error patterns
	if strings.Contains(outputLower, "connection refused") || strings.Contains(errStr, "connection refused") {
		return fmt.Errorf("mount failed: %w\nOutput: %s\nHint: Is the %s adapter running? Check with 'dfsctl adapter list'", err, output, protocol)
	}

	if strings.Contains(outputLower, "not found") || strings.Contains(outputLower, "no such file") ||
		strings.Contains(outputLower, "does not exist") {
		return fmt.Errorf("mount failed: %w\nOutput: %s\nHint: Does the share exist? Check with 'dfsctl share list'", err, output)
	}

	if strings.Contains(outputLower, "permission denied") || strings.Contains(outputLower, "operation not permitted") ||
		strings.Contains(errStr, "permission denied") {
		return fmt.Errorf("mount failed: %w\nOutput: %s\nHint: Mount commands may require sudo privileges", err, output)
	}

	if strings.Contains(outputLower, "already mounted") || strings.Contains(outputLower, "busy") {
		return fmt.Errorf("mount failed: %w\nOutput: %s\nHint: The mount point may already be in use. Check with 'mount' command", err, output)
	}

	if strings.Contains(outputLower, "authentication") || strings.Contains(outputLower, "login") ||
		strings.Contains(outputLower, "access denied") {
		return fmt.Errorf("mount failed: %w\nOutput: %s\nHint: Check your credentials with 'dfsctl login'", err, output)
	}

	if strings.Contains(outputLower, "broken pipe") || strings.Contains(outputLower, "connection reset") {
		if protocol == "SMB" {
			return fmt.Errorf("mount failed: %w\nOutput: %s\nHint: This often indicates wrong password or authentication failure. Verify your credentials and try again", err, output)
		}
		return fmt.Errorf("mount failed: %w\nOutput: %s\nHint: Server closed the connection unexpectedly. Check %s adapter logs with 'dittofs logs'", err, output, protocol)
	}

	// Generic error
	return fmt.Errorf("mount failed: %w\nOutput: %s\nHint: Server may be running on port %d. Check with 'dfsctl adapter list'", err, output, port)
}
