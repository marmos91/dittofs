// Package netgroup implements netgroup management commands for dfsctl.
package netgroup

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for netgroup management.
var Cmd = &cobra.Command{
	Use:   "netgroup",
	Short: "Manage netgroups (IP access control)",
	Long: `Create and manage netgroups for IP-based share access control. A netgroup
is a named set of IP addresses, CIDR ranges, or hostnames that can be referenced
from share security policies to allow or restrict which network endpoints can
access a share. All subcommands require admin privileges.

Examples:
  # List all netgroups
  dfsctl netgroup list

  # Create a netgroup and populate it
  dfsctl netgroup create --name office-network
  dfsctl netgroup add-member office-network --type cidr --value 192.168.1.0/24

  # Show a netgroup and its members (including member IDs)
  dfsctl netgroup show office-network

  # Remove a specific member by UUID
  dfsctl netgroup remove-member office-network --member-id <uuid>

  # Remove a netgroup (fails if still in use by shares)
  dfsctl netgroup remove office-network`,
}

func init() {
	Cmd.AddCommand(createCmd)
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(showCmd)
	Cmd.AddCommand(removeCmd)
	Cmd.AddCommand(addMemberCmd)
	Cmd.AddCommand(removeMemberCmd)
}
