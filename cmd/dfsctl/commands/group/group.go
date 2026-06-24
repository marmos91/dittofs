// Package group implements group management commands for dfsctl.
package group

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for group management.
var Cmd = &cobra.Command{
	Use:   "group",
	Short: "Group management",
	Long: `Manage Unix groups on the DittoFS server. Groups bundle users together so
that share permissions can be granted to multiple users at once using a single
group reference. Each group carries a Unix GID used for NFS uid/gid resolution.
All subcommands require admin privileges.

Examples:
  # List all groups
  dfsctl group list

  # Get group details including current members
  dfsctl group get editors

  # Create a group with an explicit GID
  dfsctl group create --name editors --gid 1001

  # Edit a group interactively
  dfsctl group edit editors

  # Add a user to a group
  dfsctl group add-user editors alice

  # Remove a user from a group
  dfsctl group remove-user editors alice

  # Delete a group (prompts for confirmation)
  dfsctl group delete editors`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(getCmd)
	Cmd.AddCommand(createCmd)
	Cmd.AddCommand(editCmd)
	Cmd.AddCommand(deleteCmd)
	Cmd.AddCommand(addUserCmd)
	Cmd.AddCommand(removeUserCmd)
}
