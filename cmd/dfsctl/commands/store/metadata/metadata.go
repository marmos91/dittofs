// Package metadata implements metadata store management commands.
package metadata

import (
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/metadata/backup"
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/metadata/repo"
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/metadata/restore"
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
  dfsctl store metadata add --name persistent-meta --type badger --config '{"path":"/data/meta"}'

  # Trigger an on-demand backup
  dfsctl store metadata fast-meta backup --repo daily-s3

  # Restore from a specific backup (after disabling shares)
  dfsctl store metadata fast-meta restore --from 01HABCDEFGHJKMNPQRSTUVWXY1

  # Add a backup repo
  dfsctl store metadata fast-meta repo add --name daily-s3 --kind s3 ...`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(addCmd)
	Cmd.AddCommand(editCmd)
	Cmd.AddCommand(removeCmd)
	Cmd.AddCommand(healthCmd)

	// Phase 6 additions — backup / restore / repo subtrees.
	Cmd.AddCommand(backup.Cmd)
	Cmd.AddCommand(repo.Cmd)
	Cmd.AddCommand(restore.Cmd)
}
