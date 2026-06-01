// Package trash implements the `dfsctl trash` command group for managing a
// share's recycle bin (#190): listing, restoring, emptying, and inspecting
// recycled roots.
package trash

import "github.com/spf13/cobra"

// Cmd is the `trash` command group, registered on the dfsctl root command.
var Cmd = &cobra.Command{
	Use:   "trash",
	Short: "Recycle-bin management",
	Long: `Manage a share's recycle bin (#recycle).

When a share has trash enabled, deleted files and directories are moved to a
per-share recycle bin instead of being purged immediately. Use these commands
to inspect, restore, or empty that bin.

Examples:
  dfsctl trash list myshare
  dfsctl trash restore myshare "#recycle/2026-06-01/report.txt"
  dfsctl trash empty myshare --force
  dfsctl trash status myshare`,
}

func init() {
	Cmd.AddCommand(listCmd, restoreCmd, emptyCmd, statusCmd)
}
