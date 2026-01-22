// Package share implements share management commands for dittofsctl.
package share

import (
	"github.com/marmos91/dittofs/cmd/dittofsctl/commands/share/permission"
	"github.com/spf13/cobra"
)

// Cmd is the parent command for share management.
var Cmd = &cobra.Command{
	Use:   "share",
	Short: "Share management",
	Long: `Manage shares on the DittoFS server.

Share commands allow you to create, list, update, and delete shares,
as well as manage share permissions.
These operations require admin privileges.

Examples:
  # List all shares
  dittofsctl share list

  # Create a new share
  dittofsctl share create --name /archive --metadata default --payload s3-store

  # Update a share
  dittofsctl share update /archive --read-only

  # Delete a share
  dittofsctl share delete /archive

  # Grant permission
  dittofsctl share permission grant /archive --user alice --level read-write`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(createCmd)
	Cmd.AddCommand(deleteCmd)
	Cmd.AddCommand(updateCmd)
	Cmd.AddCommand(permission.Cmd)
}
