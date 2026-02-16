// Package idmap implements identity mapping commands for dittofsctl.
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
  dittofsctl idmap list

  # Add a mapping
  dittofsctl idmap add --principal alice@EXAMPLE.COM --username alice

  # Remove a mapping
  dittofsctl idmap remove --principal alice@EXAMPLE.COM`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(addCmd)
	Cmd.AddCommand(removeCmd)
}
