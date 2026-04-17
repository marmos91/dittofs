// Package metadata implements metadata store management commands.
package metadata

import (
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/metadata/backup"
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/metadata/backup/restore"
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
  dfsctl store metadata fast-meta backup run --repo daily-s3

  # Restore from a specific backup (after disabling shares)
  dfsctl store metadata fast-meta backup restore --from 01HABCDEFGHJKMNPQRSTUVWXY1

  # Manage backup destination repos
  dfsctl store metadata fast-meta backup repo add --name daily-s3 --kind s3 ...

  # Inspect backup/restore job attempts
  dfsctl store metadata fast-meta backup job list`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(addCmd)
	Cmd.AddCommand(editCmd)
	Cmd.AddCommand(removeCmd)
	Cmd.AddCommand(healthCmd)

	// Phase 6: a single `backup` subtree groups run / list / show / pin /
	// unpin / restore / repo / job.
	Cmd.AddCommand(backup.Cmd)

	// restore.Cmd lives under backup.Cmd but is wired here (not in backup's
	// init) because backup/restore imports backup/ for WaitForJob — wiring
	// from backup would create an import cycle.
	backup.Cmd.AddCommand(restore.Cmd)
}
