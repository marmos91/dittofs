// Package job implements the backup-job inspection sub-tree under the
// `dfsctl store metadata <name> backup job` CLI path.
//
// Verbs:
//   - list    — filtered list of backup/restore job attempts (D-42)
//   - show    — single-job detail with progress bar (D-47)
//   - cancel  — cancel a running job (D-43/D-44/D-45)
package job

import "github.com/spf13/cobra"

// Cmd is the `backup job` parent. Attached to backup.Cmd by
// `cmd/dfsctl/commands/store/metadata/backup/backup.go`.
var Cmd = &cobra.Command{
	Use:   "job",
	Short: "Inspect backup/restore job attempts",
	Long: `Inspect backup and restore job attempts for a metadata store.

Examples:
  # List jobs (filterable by status/kind/repo/limit — D-42)
  dfsctl store metadata fast-meta backup job list --status running

  # Show job detail (grouped sections + progress bar when running — D-47)
  dfsctl store metadata fast-meta backup job show 01HABCDEFGHJKMNPQRST

  # Cancel a running job (idempotent on terminal — D-45)
  dfsctl store metadata fast-meta backup job cancel 01HABCDEFGHJKMNPQRST`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(showCmd)
	Cmd.AddCommand(cancelCmd)
}
