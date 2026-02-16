// Package backup implements backup subcommands for dittofs.
package backup

import (
	"github.com/spf13/cobra"
)

// Cmd is the backup subcommand.
var Cmd = &cobra.Command{
	Use:   "backup",
	Short: "Backup operations",
	Long: `Backup DittoFS data stores.

Subcommands:
  controlplane  Backup control plane database`,
}

func init() {
	Cmd.AddCommand(controlplaneCmd)
}
