// Package user implements user management commands for dittofsctl.
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
  dittofsctl user list

  # Create a new user interactively
  dittofsctl user create

  # Create a user with flags
  dittofsctl user create --username alice --password secret --role user

  # Edit a user interactively
  dittofsctl user edit alice

  # Delete a user
  dittofsctl user delete alice`,
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
