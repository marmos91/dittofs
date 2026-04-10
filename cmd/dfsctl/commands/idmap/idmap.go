// Package idmap implements identity mapping commands for dfsctl.
package idmap

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for identity mapping management.
var Cmd = &cobra.Command{
	Use:   "idmap",
	Short: "Manage identity mappings",
	Long: `Manage identity mappings (authentication principal to control plane user).

Identity mappings allow you to associate authentication principals with
local DittoFS user accounts. This works across protocols:

  NFS/Kerberos:  alice@EXAMPLE.COM
  SMB/NTLM:      CORP\alice
  SMB/Kerberos:  alice@CORP.COM

Mappings are shared across NFS and SMB, ensuring consistent uid/gid
resolution in mixed-protocol deployments.

Examples:
  # List all identity mappings
  dfsctl idmap list

  # Map a Kerberos principal (works for both NFS and SMB)
  dfsctl idmap add --principal alice@EXAMPLE.COM --username alice

  # Map an NTLM domain user
  dfsctl idmap add --principal 'CORP\alice' --username alice

  # Remove a mapping
  dfsctl idmap remove --principal alice@EXAMPLE.COM`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(addCmd)
	Cmd.AddCommand(removeCmd)
}
