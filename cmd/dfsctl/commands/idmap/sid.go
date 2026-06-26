package idmap

import (
	"github.com/spf13/cobra"
)

// sidCmd is the parent command for foreign-SID idmap allocation management.
var sidCmd = &cobra.Command{
	Use:   "sid",
	Short: "Manage foreign-SID UID/GID allocations",
	Long: `Manage durable foreign-SID to Unix UID/GID allocations. When DittoFS
resolves Active Directory or LDAP principals, foreign domain SIDs (of the form
` + "`S-1-5-21-<domain>-<rid>`" + `) are bound to stable Unix UIDs and GIDs exactly
once and never remapped, ensuring a foreign SID always resolves to the same
numeric identity across restarts.

This subcommand surfaces that allocation table for administrative inspection and
cleanup. It is distinct from "dfsctl idmap add/list/remove", which manages the
authentication-principal to local-user mappings used during login.

Examples:
  # List all foreign-SID allocations
  dfsctl idmap sid list

  # Output the allocation table as JSON
  dfsctl idmap sid list -o json

  # Remove a misallocated SID entry (use with care)
  dfsctl idmap sid remove S-1-5-21-111-222-333-1107`,
}

func init() {
	sidCmd.AddCommand(sidListCmd)
	sidCmd.AddCommand(sidRemoveCmd)
	Cmd.AddCommand(sidCmd)
}
