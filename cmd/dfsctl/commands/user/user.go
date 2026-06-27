// Package user implements user management commands for dfsctl.
package user

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for user management.
var Cmd = &cobra.Command{
	Use:   "user",
	Short: "User management",
	Long: `Manage local user accounts on the DittoFS server. Local users are
distinct from identities resolved via Kerberos or LDAP — they are accounts
stored in the DittoFS control plane and used for direct authentication.
Most subcommands require admin privileges; "change-password" operates on the
currently authenticated account and is available to all users.

Examples:
  # List all registered users
  dfsctl user list

  # Create a new user interactively
  dfsctl user create

  # Create a user with an explicit UID for NFS access
  dfsctl user create --username alice --password secret --uid 1000 --role user

  # Edit a user's group membership
  dfsctl user edit alice --groups editors,viewers

  # Reset a user's password as an admin
  dfsctl user password alice

  # Remove a user (prompts for confirmation)
  dfsctl user remove alice`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(createCmd)
	Cmd.AddCommand(removeCmd)
	Cmd.AddCommand(getCmd)
	Cmd.AddCommand(editCmd)
	Cmd.AddCommand(passwordCmd)
	Cmd.AddCommand(changePasswordCmd)
}
