// Package group implements group management commands for dittofsctl.
package group

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for group management.
var Cmd = &cobra.Command{
	Use:   "group",
	Short: "Group management",
	Long: `Manage groups on the DittoFS server.

Group commands allow you to create, list, get, edit, and delete groups,
as well as manage group membership.
These operations require admin privileges.

Examples:
  # List all groups
  dittofsctl group list

  # Get group details
  dittofsctl group get admins

  # Create a new group
  dittofsctl group create --name editors

  # Edit a group interactively
  dittofsctl group edit editors

  # Add a user to a group
  dittofsctl group add-user editors alice

  # Remove a user from a group
  dittofsctl group remove-user editors alice

  # Delete a group
  dittofsctl group delete editors`,
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
