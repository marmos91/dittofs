// Package grace implements NFSv4 grace period management commands.
package grace

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for NFSv4 grace period management.
var Cmd = &cobra.Command{
	Use:   "grace",
	Short: "Manage NFSv4 grace period",
	Long: `Manage the NFSv4 grace period on the DittoFS server.

Grace period commands allow you to monitor and control the NFSv4 grace
period that occurs after server restart. During the grace period, clients
reclaim their previously-held state (open files, locks).

Examples:
  # Check grace period status
  dfsctl grace status

  # Check status in JSON format
  dfsctl grace status -o json

  # Force-end the grace period
  dfsctl grace end`,
}

func init() {
	Cmd.AddCommand(statusCmd)
	Cmd.AddCommand(endCmd)
}
