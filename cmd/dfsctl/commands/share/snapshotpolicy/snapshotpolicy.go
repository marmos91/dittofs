// Package snapshotpolicy implements share snapshot policy commands
// (schedule + retention) for dfsctl.
package snapshotpolicy

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for share snapshot policy management.
var Cmd = &cobra.Command{
	Use:   "snapshot-policy",
	Short: "Manage scheduled snapshot policies (schedule + retention)",
	Long: `Manage per-share snapshot policies.

A snapshot policy makes a share snapshot itself automatically on a fixed
interval and prunes old scheduler-created snapshots past the retention
bounds (keep-last and/or ttl). Manually-created snapshots are never pruned.

Examples:
  # Daily snapshots, keep the newest 7, drop anything older than 30 days
  dfsctl share snapshot-policy set /archive --interval @daily --keep-last 7 --ttl 720h

  # Show a share's policy
  dfsctl share snapshot-policy show /archive

  # List every policy
  dfsctl share snapshot-policy list

  # Trigger the policy immediately, ignoring its interval
  dfsctl share snapshot-policy run /archive

  # Remove a share's policy
  dfsctl share snapshot-policy delete /archive`,
}

func init() {
	Cmd.AddCommand(setCmd)
	Cmd.AddCommand(showCmd)
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(deleteCmd)
	Cmd.AddCommand(runCmd)
}
