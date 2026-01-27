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

Adapter commands allow you to list, enable, disable, and edit protocol adapters.
These operations require admin privileges.

Examples:
  # List adapters
  dittofsctl adapter list

  # Enable NFS adapter on port 12049
  dittofsctl adapter enable nfs --port 12049

  # Disable SMB adapter
  dittofsctl adapter disable smb

  # Edit adapter interactively
  dittofsctl adapter edit nfs

  # Edit adapter settings with flags
  dittofsctl adapter edit nfs --port 3049`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(enableCmd)
	Cmd.AddCommand(disableCmd)
	Cmd.AddCommand(editCmd)
}
