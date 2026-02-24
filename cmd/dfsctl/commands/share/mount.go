package share

import (
	"fmt"
	"os"
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

Note: Mount commands typically require sudo/root privileges on Unix systems.

Platform differences for SMB with sudo:
  - Linux: Mount owner set to your user via uid/gid options (default mode 0755)
  - macOS: Mount owned by root (uid/gid removed in Catalina), default mode 0777
  - macOS alternative: mount to ~/mnt without sudo for user-owned mount
  - Windows: Uses 'net use' to map network drives (e.g., dfsctl share mount --protocol smb /export Z:)`,
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

func runMount(cmd *cobra.Command, args []string) error {
	sharePath := args[0]
	mountPoint := args[1]

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

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return fmt.Errorf("failed to get authenticated client: %w\nHint: Run 'dfsctl login' first", err)
	}

	adapters, err := client.ListAdapters()
	if err != nil {
		return fmt.Errorf("failed to list adapters: %w\nHint: Is the DittoFS server running?", err)
	}

	switch protocol {
	case "nfs":
		return mountNFS(sharePath, mountPoint, adapters)
	default:
		return mountSMB(sharePath, mountPoint, adapters)
	}
}

func getAdapterPort(adapters []apiclient.Adapter, protocol string, defaultPort int) int {
	for _, adapter := range adapters {
		if strings.EqualFold(adapter.Type, protocol) && adapter.Enabled {
			return adapter.Port
		}
	}
	return defaultPort
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

func formatMountError(err error, output, protocol string, port int) error {
	combined := strings.ToLower(output + " " + err.Error())

	// Match known error patterns to user-friendly hints.
	hints := []struct {
		keywords []string
		hint     string
	}{
		{[]string{"connection refused"}, fmt.Sprintf("Is the %s adapter running? Check with 'dfsctl adapter list'", protocol)},
		{[]string{"not found", "no such file", "does not exist"}, "Does the share exist? Check with 'dfsctl share list'"},
		{[]string{"permission denied", "operation not permitted"}, "Mount commands may require sudo privileges"},
		{[]string{"already mounted", "busy"}, "The mount point may already be in use. Check with 'mount' command"},
		{[]string{"authentication", "login", "access denied"}, "Check your credentials with 'dfsctl login'"},
	}

	for _, h := range hints {
		for _, kw := range h.keywords {
			if strings.Contains(combined, kw) {
				return fmt.Errorf("mount failed: %w\nOutput: %s\nHint: %s", err, output, h.hint)
			}
		}
	}

	// Broken pipe / connection reset has protocol-specific hints.
	if strings.Contains(combined, "broken pipe") || strings.Contains(combined, "connection reset") {
		if protocol == "SMB" {
			return fmt.Errorf("mount failed: %w\nOutput: %s\nHint: This often indicates wrong password or authentication failure. Verify your credentials and try again", err, output)
		}
		return fmt.Errorf("mount failed: %w\nOutput: %s\nHint: Server closed the connection unexpectedly. Check %s adapter logs with 'dittofs logs'", err, output, protocol)
	}

	return fmt.Errorf("mount failed: %w\nOutput: %s\nHint: Server may be running on port %d. Check with 'dfsctl adapter list'", err, output, port)
}
