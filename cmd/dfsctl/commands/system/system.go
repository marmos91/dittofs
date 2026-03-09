// Package system implements system-level management commands.
package system

import "github.com/spf13/cobra"

// Cmd is the root command for system operations.
var Cmd = &cobra.Command{
	Use:   "system",
	Short: "System operations",
	Long:  `System-level operations for managing the DittoFS server.`,
}

func init() {
	Cmd.AddCommand(drainUploadsCmd)
}
