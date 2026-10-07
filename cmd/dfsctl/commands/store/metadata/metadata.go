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
The only store type is badger (BadgerDB, on disk or in memory).

Examples:
  # List metadata stores
  dfsctl store metadata list

  # Add a BadgerDB store
  dfsctl store metadata add --name persistent-meta --db-path /data/meta

  # Add an in-memory store
  dfsctl store metadata add --name scratch-meta --in-memory`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(addCmd)
	Cmd.AddCommand(editCmd)
	Cmd.AddCommand(removeCmd)
	Cmd.AddCommand(healthCmd)
	Cmd.AddCommand(recomputeUsageCmd)
}
