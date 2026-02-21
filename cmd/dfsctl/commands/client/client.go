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

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(evictCmd)
}
