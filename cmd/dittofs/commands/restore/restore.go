// Package restore implements restore subcommands for dittofs.
package restore

import (
	"github.com/spf13/cobra"
)

// Cmd is the restore subcommand.
var Cmd = &cobra.Command{
	Use:   "restore",
	Short: "Restore operations",
	Long: `Restore DittoFS data stores from backups.

Subcommands:
  controlplane  Restore control plane database from backup`,
}

func init() {
	Cmd.AddCommand(controlplaneCmd)
}
