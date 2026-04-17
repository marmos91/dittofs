// Package share implements share management commands for dfsctl.
package share

import (
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/share/permission"
	"github.com/spf13/cobra"
)

// Cmd is the parent command for share management.
var Cmd = &cobra.Command{
	Use:   "share",
	Short: "Share management",
	Long: `Manage shares on the DittoFS server.

The ` + "`share`" + ` tree uses ` + "`share <verb> <name>`" + ` — Cobra
resolves the verb as the subcommand and ` + "`<name>`" + ` as its positional
argument. These operations require admin privileges.

Examples:
  # List all shares
  dfsctl share list

  # Create a new share
  dfsctl share create --name /archive --metadata default --local fs-cache --remote s3-store

  # Show share details
  dfsctl share show /archive

  # Edit a share interactively
  dfsctl share edit /archive

  # Edit a share with flags
  dfsctl share edit /archive --read-only true

  # Disable a share (drain clients, block new connections)
  dfsctl share disable /archive

  # Re-enable a share
  dfsctl share enable /archive

  # Delete a share
  dfsctl share delete /archive

  # Grant permission
  dfsctl share permission grant /archive --user alice --level read-write`,
}

func init() {
	// Root-level verbs (no target name — D-35 canonical shape)
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(createCmd)
	Cmd.AddCommand(permission.Cmd) // nested sub-tree keeps its own shape
	Cmd.AddCommand(listMountsCmd)  // list-mounts is a list, not a per-share verb

	// Per-share verbs — each leaf uses cobra.ExactArgs(1) with args[0] = <name>
	Cmd.AddCommand(showCmd)
	Cmd.AddCommand(editCmd)
	Cmd.AddCommand(deleteCmd)
	Cmd.AddCommand(mountCmd)
	Cmd.AddCommand(unmountCmd)

	// Phase 6 additions
	Cmd.AddCommand(disableCmd)
	Cmd.AddCommand(enableCmd)
}
