// Package client implements NFS client management commands.
package client

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for NFS client management.
var Cmd = &cobra.Command{
	Use:   "client",
	Short: "NFS client management",
	Long: `Manage connected NFS clients on the DittoFS server.

Client commands allow you to list connected NFS clients and evict
misbehaving ones. These operations require admin privileges.

Examples:
  # List connected clients
  dfsctl client list

  # List clients in JSON format
  dfsctl client list -o json

  # Evict a client by ID
  dfsctl client evict 0000000100000001`,
}

// sessionsCmd is the parent command for client session management.
var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Manage client sessions",
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
	Cmd.AddCommand(evictCmd)
	Cmd.AddCommand(sessionsCmd)
}
