// Package adapter implements protocol adapter management commands.
package adapter

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for adapter management.
var Cmd = &cobra.Command{
	Use:   "adapter",
	Short: "Protocol adapter management",
	Long: `Manage protocol adapters (NFS and SMB) on the DittoFS server.

Protocol adapters control which wire protocols the server accepts connections on and on which ports. Use these commands to enable, disable, or reconfigure adapters without restarting the server. All operations require admin privileges.

Examples:
  # List all adapters with their current status and ports
  dfsctl adapter list

  # Enable the NFS adapter on the default port
  dfsctl adapter enable nfs

  # Enable the SMB adapter on port 12445
  dfsctl adapter enable smb --port 12445

  # Disable the NFS adapter
  dfsctl adapter disable nfs

  # Tune NFS adapter settings (portmapper, lease time, etc.)
  dfsctl adapter settings nfs update --portmapper-enabled --portmapper-port 10111`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(enableCmd)
	Cmd.AddCommand(disableCmd)
	Cmd.AddCommand(editCmd)
	Cmd.AddCommand(settingsCmd)
}
