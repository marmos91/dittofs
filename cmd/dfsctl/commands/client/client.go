// Package client implements unified client management commands.
package client

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for client management.
var Cmd = &cobra.Command{
	Use:   "client",
	Short: "Manage connected clients",
	Long: `Manage connected NFS and SMB clients on the DittoFS server.

Use these commands to inspect which clients are currently connected, filter by protocol or share, and forcefully disconnect misbehaving sessions. All operations require admin privileges.

Examples:
  # List all connected clients across NFS and SMB
  dfsctl client list

  # Show only NFS clients
  dfsctl client list --protocol nfs

  # Show clients connected to a specific share
  dfsctl client list --share myshare

  # Disconnect a specific client by its ID
  dfsctl client disconnect nfs-42 --force`,
}

// sessionsCmd is the parent command for NFS client session management.
var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Manage NFS client sessions",
	Long: `Manage NFSv4.1 sessions for a specific connected NFS client.

Each NFSv4.1 client may have one or more sessions, each with its own fore and back channel slot tables. Use these commands to inspect session state and force-destroy sessions that are stuck or misbehaving. Admin privileges are required.

Examples:
  # List all sessions for a client (use the hex client ID from 'client list')
  dfsctl client sessions list 0000000100000001

  # Force-destroy a session that is stuck
  dfsctl client sessions destroy 0000000100000001 a1b2c3d4e5f6a7b8 --force`,
}

func init() {
	sessionsCmd.AddCommand(sessionsListCmd)
	sessionsCmd.AddCommand(sessionsDestroyCmd)

	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(disconnectCmd)
	Cmd.AddCommand(sessionsCmd)
}
