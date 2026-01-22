// Package adapter implements protocol adapter management commands.
package adapter

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for adapter management.
var Cmd = &cobra.Command{
	Use:   "adapter",
	Short: "Protocol adapter management",
	Long: `Manage protocol adapters on the DittoFS server.

Adapter commands allow you to list, add, update, and remove protocol adapters.
These operations require admin privileges.

Examples:
  # List adapters
  dittofsctl adapter list

  # Update NFS adapter port
  dittofsctl adapter update nfs --port 3049

  # Enable/disable adapter
  dittofsctl adapter update smb --enabled false`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(updateCmd)
}
