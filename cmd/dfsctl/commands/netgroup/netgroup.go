// Package netgroup implements netgroup management commands for dfsctl.
package netgroup

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for netgroup management.
var Cmd = &cobra.Command{
	Use:   "netgroup",
	Short: "Manage netgroups (IP access control)",
	Long: `Create and manage netgroups for IP-based share access control.

Netgroups define sets of IP addresses, CIDR ranges, or hostnames that can
be referenced from share security policies to control access.

Examples:
  # List all netgroups
  dfsctl netgroup list

  # Create a netgroup
  dfsctl netgroup create --name office-network

  # Show netgroup details
  dfsctl netgroup show office-network

  # Add a member
  dfsctl netgroup add-member office-network --type cidr --value 192.168.1.0/24

  # Remove a member
  dfsctl netgroup remove-member office-network --member-id <uuid>

  # Delete a netgroup
  dfsctl netgroup delete office-network`,
}

func init() {
	Cmd.AddCommand(createCmd)
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(showCmd)
	Cmd.AddCommand(deleteCmd)
	Cmd.AddCommand(addMemberCmd)
	Cmd.AddCommand(removeMemberCmd)
}
