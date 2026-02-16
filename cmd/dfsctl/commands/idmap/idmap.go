// Package idmap implements identity mapping commands for dfsctl.
package idmap

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for identity mapping management.
var Cmd = &cobra.Command{
	Use:   "idmap",
	Short: "Manage identity mappings",
	Long: `Manage identity mappings (NFSv4 principal to control plane user).

Identity mappings allow you to associate Kerberos or NFSv4 principals
(e.g., "alice@EXAMPLE.COM") with local DittoFS user accounts.

These mappings are used for ACL evaluation and identity resolution.

Examples:
  # List all identity mappings
  dfsctl idmap list

  # Add a mapping
  dfsctl idmap add --principal alice@EXAMPLE.COM --username alice

  # Remove a mapping
  dfsctl idmap remove --principal alice@EXAMPLE.COM`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(addCmd)
	Cmd.AddCommand(removeCmd)
}
