// Package metadata implements metadata store management commands.
package metadata

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for metadata store management.
var Cmd = &cobra.Command{
	Use:   "metadata",
	Short: "Manage metadata stores",
	Long: `Manage metadata stores on the DittoFS server.

Metadata stores hold file system structure, attributes, and permissions.
Supported types: memory, badger, postgres

Examples:
  # List metadata stores
  dfsctl store metadata list

  # Add a memory store
  dfsctl store metadata add --name fast-meta --type memory

  # Add a BadgerDB store
  dfsctl store metadata add --name persistent-meta --type badger --config '{"path":"/data/meta"}'`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(addCmd)
	Cmd.AddCommand(editCmd)
	Cmd.AddCommand(removeCmd)
}
