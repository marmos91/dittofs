// Package settings implements server settings management commands.
package settings

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for settings management.
var Cmd = &cobra.Command{
	Use:   "settings",
	Short: "Server settings management",
	Long: `Manage live server settings on the DittoFS server.

Server settings are key-value pairs that control runtime behaviour (logging level, feature flags, etc.) without requiring a restart. List all available keys with 'settings list', inspect a single value with 'settings get', and change it with 'settings set'. All operations require admin privileges.

Examples:
  # List every setting with its current value and description
  dfsctl settings list

  # Inspect the current logging level
  dfsctl settings get logging.level

  # Switch to debug logging at runtime
  dfsctl settings set logging.level DEBUG`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(getCmd)
	Cmd.AddCommand(setCmd)
}
