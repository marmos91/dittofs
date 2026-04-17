package backup

import (
	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/metadata/backup/job"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

// clientFactory returns the authenticated apiclient.Client. Swapped in tests
// to inject an httptest-backed fake without touching the real credential
// store. Production path falls through to cmdutil.GetAuthenticatedClient.
var clientFactory = cmdutil.GetAuthenticatedClient

func getClient() (*apiclient.Client, error) { return clientFactory() }

// Cmd is the per-store backup parent. Attached to metadata.Cmd in Plan 06 —
// this package explicitly does NOT modify metadata.go to avoid a Wave-3
// parallel-agent conflict.
//
// Invoked as `dfsctl store metadata <name> backup [--repo <name>] [...]`; with
// no subcommand the parent runs runRun (the on-demand trigger) per 06-PATTERNS
// D-Claude discretion.
var Cmd = &cobra.Command{
	Use:   "backup",
	Short: "Manage backups for a metadata store",
	Long: `Manage backups for a metadata store on the DittoFS server.

Examples:
  # Trigger on-demand backup (blocks until terminal state — D-01)
  dfsctl store metadata fast-meta backup --repo daily-s3

  # Return immediately with the job record
  dfsctl store metadata fast-meta backup --repo daily-s3 --async

  # List backup records (D-26 columns: ID | CREATED | SIZE | STATUS | REPO | PINNED)
  dfsctl store metadata fast-meta backup list --repo daily-s3

  # Show a specific record (D-48)
  dfsctl store metadata fast-meta backup show 01HABCDEFGHJKMNPQRST

  # Pin / unpin a record (D-23)
  dfsctl store metadata fast-meta backup pin 01HABCDEFGHJKMNPQRST
  dfsctl store metadata fast-meta backup unpin 01HABCDEFGHJKMNPQRST`,
	Args: cobra.ExactArgs(1), // <store-name>
	RunE: runRun,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(showCmd)
	Cmd.AddCommand(pinCmd)
	Cmd.AddCommand(unpinCmd)
	Cmd.AddCommand(job.Cmd)
	registerRunFlags(Cmd)
}
