// Package context implements context management subcommands for dfsctl.
package context

import (
	"github.com/spf13/cobra"
)

// Cmd is the context subcommand.
var Cmd = &cobra.Command{
	Use:   "context",
	Short: "Manage server contexts",
	Long: `Manage named connection contexts for one or more DittoFS servers.

Each context stores a server URL, authentication credentials, and a display name. Contexts work similarly to kubectl contexts: log in once per server, then switch between them with 'context use'. All subsequent dfsctl commands use the active context automatically.

Examples:
  # List all saved contexts
  dfsctl context list

  # Switch to a context named "production"
  dfsctl context use production

  # Show which context is currently active
  dfsctl context current

  # Remove a context that is no longer needed
  dfsctl context remove staging`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(useCmd)
	Cmd.AddCommand(currentCmd)
	Cmd.AddCommand(renameCmd)
	Cmd.AddCommand(removeCmd)
}
