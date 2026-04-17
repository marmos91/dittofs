package backup

import (
	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/metadata/backup/job"
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/metadata/backup/repo"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

// NOTE on import cycle:
// The `backup/restore` subpackage imports this package (for WaitForJob + poll
// sentinels), so we cannot import `backup/restore` here. Instead, metadata.go
// wires `restore.Cmd` onto `backup.Cmd` at its init() — both packages are
// imported directly from metadata without cycling through this package.

// clientFactory returns the authenticated apiclient.Client. Swapped in tests
// to inject an httptest-backed fake without touching the real credential
// store. Production path falls through to cmdutil.GetAuthenticatedClient.
var clientFactory = cmdutil.GetAuthenticatedClient

func getClient() (*apiclient.Client, error) { return clientFactory() }

// Cmd is the per-store backup parent. Groups every backup-related verb:
// trigger (run), record management (list/show/pin/unpin), destination repos
// (repo add/edit/list/show/remove), restore (restore), and job inspection
// (job list/show/cancel).
//
// Invoked as `dfsctl store metadata <store> backup <verb> [...]`.
var Cmd = &cobra.Command{
	Use:   "backup",
	Short: "Manage backups, restores, and backup repos for a metadata store",
	Long: `Manage backups, restores, and backup repos for a metadata store.

Examples:
  # Trigger on-demand backup (blocks until terminal state — D-01)
  dfsctl store metadata fast-meta backup run --repo daily-s3

  # Return immediately with the job record
  dfsctl store metadata fast-meta backup run --repo daily-s3 --async

  # List backup records
  dfsctl store metadata fast-meta backup list --repo daily-s3

  # Restore from the latest succeeded backup (after disabling shares)
  dfsctl store metadata fast-meta backup restore

  # Manage backup destination repos
  dfsctl store metadata fast-meta backup repo list
  dfsctl store metadata fast-meta backup repo add --name daily-s3 --kind s3 ...

  # Inspect job attempts
  dfsctl store metadata fast-meta backup job list`,
}

func init() {
	Cmd.AddCommand(runCmd)
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(showCmd)
	Cmd.AddCommand(pinCmd)
	Cmd.AddCommand(unpinCmd)
	Cmd.AddCommand(job.Cmd)
	Cmd.AddCommand(repo.Cmd)
	// restore.Cmd is wired from metadata.init() to avoid a backup ⇄ restore
	// import cycle (restore imports this package for WaitForJob).
}
