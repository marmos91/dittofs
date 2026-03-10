// Package local implements local block store management commands.
package local

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for local block store management.
var Cmd = &cobra.Command{
	Use:   "local",
	Short: "Local block store management",
	Long: `Manage local block stores on the DittoFS server.

Local block stores provide fast disk-backed storage for file content blocks.
Supported types: fs (filesystem), memory (testing)

Examples:
  # List local block stores
  dfsctl store block local list

  # Add a filesystem block store
  dfsctl store block local add --name fs-cache --type fs --config '{"path":"/data/blocks"}'

  # Add a memory block store (for testing)
  dfsctl store block local add --name test-local --type memory`,
}

func init() {
	Cmd.AddCommand(addCmd)
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(editCmd)
	Cmd.AddCommand(removeCmd)
}
