// Package repo implements backup repo management commands under a metadata store.
package repo

import "github.com/spf13/cobra"

// Cmd is the parent command for backup repo management.
var Cmd = &cobra.Command{
	Use:   "repo",
	Short: "Manage backup repos attached to a metadata store",
	Long: `Manage backup repos attached to a metadata store.

A backup repo declares where backups are stored (destination kind: local or s3),
optionally a cron schedule, retention policy, and encryption settings.

Examples:
  # List repos on a store
  dfsctl store metadata fast-meta repo list

  # Add an S3 repo (flag driven)
  dfsctl store metadata fast-meta repo add --name daily-s3 --kind s3 \
    --bucket my-backups --region us-east-1 \
    --schedule "CRON_TZ=UTC 0 2 * * *" --keep-count 7

  # Edit retention
  dfsctl store metadata fast-meta repo edit daily-s3 --keep-count 14

  # Remove repo (keeps archive files)
  dfsctl store metadata fast-meta repo remove daily-s3

  # Remove repo and cascade-delete archive files
  dfsctl store metadata fast-meta repo remove daily-s3 --purge-archives`,
}

func init() {
	// Each verb registers itself in its own file's init() — keeps the
	// parent command agnostic to verb composition order.
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(showCmd)
}
