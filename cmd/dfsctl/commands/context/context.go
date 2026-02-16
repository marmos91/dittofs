// Package context implements context management subcommands for dfsctl.
package context

import (
	"github.com/spf13/cobra"
)

// Cmd is the context subcommand.
var Cmd = &cobra.Command{
	Use:   "context",
	Short: "Manage server contexts",
	Long: `Manage connection contexts for multiple DittoFS servers.

Contexts allow you to save and switch between different server configurations,
similar to kubectl contexts.

Subcommands:
  list     List all configured contexts
  use      Switch to a different context
  current  Show current context
  rename   Rename a context
  delete   Delete a context`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(useCmd)
	Cmd.AddCommand(currentCmd)
	Cmd.AddCommand(renameCmd)
	Cmd.AddCommand(deleteCmd)
}
