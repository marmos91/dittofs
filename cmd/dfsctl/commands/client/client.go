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

Client commands allow you to list connected clients across all protocols
and disconnect misbehaving ones. These operations require admin privileges.

Examples:
  # List all connected clients
  dfsctl client list

  # List only NFS clients
  dfsctl client list --protocol nfs

  # List clients on a specific share
  dfsctl client list --share /export

  # Disconnect a client by ID
  dfsctl client disconnect nfs-42`,
}

// sessionsCmd is the parent command for NFS client session management.
var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Manage NFS client sessions",
	Long: `Manage NFSv4.1 sessions for a connected NFS client.

Session commands allow you to list active sessions and force-destroy
misbehaving sessions. These operations require admin privileges.

Examples:
  # List sessions for a client
  dfsctl client sessions list 0000000100000001

  # Force-destroy a session
  dfsctl client sessions destroy 0000000100000001 a1b2c3d4...`,
}

func init() {
	sessionsCmd.AddCommand(sessionsListCmd)
	sessionsCmd.AddCommand(sessionsDestroyCmd)

	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(disconnectCmd)
	Cmd.AddCommand(sessionsCmd)
}
