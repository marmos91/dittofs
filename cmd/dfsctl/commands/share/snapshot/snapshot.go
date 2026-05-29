// Package snapshot implements share snapshot management commands.
package snapshot

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for share snapshot management.
var Cmd = &cobra.Command{
	Use:   "snapshot",
	Short: "Manage share snapshots (create, list, show, delete, restore)",
	Long: `Manage share snapshots.

A snapshot captures the full state of a share at a point in time. It can
be inspected, listed, deleted, or restored back onto a (disabled) share.

Examples:
  # Create a snapshot and wait for it to be ready
  dfsctl share snapshot create /archive --name weekly

  # List snapshots for a share
  dfsctl share snapshot list /archive

  # Show details of a single snapshot
  dfsctl share snapshot show /archive snap-abc123

  # Delete a snapshot (prompts for confirmation)
  dfsctl share snapshot delete /archive snap-abc123

  # Restore a snapshot onto a disabled share
  dfsctl share disable /archive
  dfsctl share snapshot restore /archive snap-abc123`,
}

func init() {
	Cmd.AddCommand(createCmd)
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(showCmd)
	Cmd.AddCommand(deleteCmd)
	Cmd.AddCommand(restoreCmd)
}
