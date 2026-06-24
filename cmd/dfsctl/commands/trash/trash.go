// Package trash implements the `dfsctl trash` command group for managing a
// share's recycle bin (#190): listing, restoring, emptying, and inspecting
// recycled roots.
package trash

import "github.com/spf13/cobra"

// Cmd is the `trash` command group, registered on the dfsctl root command.
var Cmd = &cobra.Command{
	Use:   "trash",
	Short: "Recycle-bin management",
	Long: `Manage a share's recycle bin (the #recycle virtual directory).

When trash is enabled on a share, deleted files and directories are moved to a per-share recycle bin instead of being permanently purged. Use these commands to inspect what is in the bin, restore individual items to their original location, or purge the bin entirely.

Examples:
  # See what is in the recycle bin for a share
  dfsctl trash list myshare

  # Restore a recycled file to its original path
  dfsctl trash restore myshare "#recycle/2026-06-01/report.txt"

  # Restore a file to a different path
  dfsctl trash restore myshare "#recycle/2026-06-01/report.txt" --to /archive/report.txt

  # Permanently empty the recycle bin
  dfsctl trash empty myshare --force`,
}

func init() {
	Cmd.AddCommand(listCmd, restoreCmd, emptyCmd, statusCmd)
}
