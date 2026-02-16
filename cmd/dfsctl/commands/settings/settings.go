// Package settings implements server settings management commands.
package settings

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for settings management.
var Cmd = &cobra.Command{
	Use:   "settings",
	Short: "Server settings management",
	Long: `Manage server settings on the DittoFS server.

Settings commands allow you to get, set, and list server configuration settings.
These operations require admin privileges.

Examples:
  # List all settings
  dfsctl settings list

  # Get a specific setting
  dfsctl settings get logging.level

  # Set a setting value
  dfsctl settings set logging.level DEBUG`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(getCmd)
	Cmd.AddCommand(setCmd)
}
