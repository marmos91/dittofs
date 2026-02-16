// Package user implements user management commands for dfsctl.
package user

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for user management.
var Cmd = &cobra.Command{
	Use:   "user",
	Short: "User management",
	Long: `Manage users on the DittoFS server.

User commands allow you to create, list, edit, and delete users.
These operations require admin privileges.

Examples:
  # List all users
  dfsctl user list

  # Create a new user interactively
  dfsctl user create

  # Create a user with flags
  dfsctl user create --username alice --password secret --role user

  # Edit a user interactively
  dfsctl user edit alice

  # Delete a user
  dfsctl user delete alice`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(createCmd)
	Cmd.AddCommand(deleteCmd)
	Cmd.AddCommand(getCmd)
	Cmd.AddCommand(editCmd)
	Cmd.AddCommand(passwordCmd)
	Cmd.AddCommand(changePasswordCmd)
}
