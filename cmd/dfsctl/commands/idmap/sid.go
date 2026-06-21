package idmap

import (
	"github.com/spf13/cobra"
)

// sidCmd is the parent command for foreign-SID idmap allocation management.
var sidCmd = &cobra.Command{
	Use:   "sid",
	Short: "Manage foreign-SID UID/GID allocations",
	Long: `Manage durable foreign-SID to Unix UID/GID allocations.

When DittoFS resolves Active Directory / LDAP principals, foreign domain SIDs
(of the form S-1-5-21-<domain>-<rid>) are durably bound to stable Unix UIDs and
GIDs. These bindings are allocated exactly once and never remapped, so a foreign
SID always resolves to the same identity.

This command surfaces that allocation table for administrative inspection and
cleanup. It is distinct from "dfsctl idmap add/list/remove", which manages the
authentication-principal to DittoFS-user mappings.

Deletion is an administrative escape hatch: removing a mapping allows a foreign
SID to be re-allocated to a different UID/GID on its next resolution, which can
re-attribute files owned by the old UID. Use with care.

Examples:
  # List all foreign-SID allocations
  dfsctl idmap sid list

  # Delete a foreign-SID allocation
  dfsctl idmap sid delete S-1-5-21-111-222-333-1107`,
}

func init() {
	sidCmd.AddCommand(sidListCmd)
	sidCmd.AddCommand(sidDeleteCmd)
	Cmd.AddCommand(sidCmd)
}
