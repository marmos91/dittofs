// Package idmap implements identity mapping commands for dfsctl.
package idmap

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for identity mapping management.
var Cmd = &cobra.Command{
	Use:   "idmap",
	Short: "Manage identity mappings",
	Long: `Manage identity mappings that link external authentication principals to
local DittoFS user accounts. Mappings are shared across NFS and SMB, ensuring
consistent uid/gid resolution in mixed-protocol deployments. Supported principal
formats include:

  NFS/Kerberos:  alice@EXAMPLE.COM
  SMB/NTLM:      CORP\alice
  SMB/Kerberos:  alice@CORP.COM

Use "dfsctl idmap sid" to inspect the separate table of foreign-SID to
Unix UID/GID allocations managed automatically by Active Directory resolution.

Examples:
  # List all identity mappings
  dfsctl idmap list

  # Map a Kerberos principal (works for both NFS and SMB)
  dfsctl idmap add --principal alice@EXAMPLE.COM --username alice

  # Map an NTLM domain user to the same local account
  dfsctl idmap add --provider ad --principal 'CORP\alice' --username alice

  # Remove a mapping (prompts for confirmation)
  dfsctl idmap remove --principal alice@EXAMPLE.COM`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(addCmd)
	Cmd.AddCommand(removeCmd)
}
