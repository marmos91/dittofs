package repo

import (
	"fmt"
	"io"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	removePurgeArchives bool
	removeForce         bool
)

var removeCmd = &cobra.Command{
	Use:   "remove <store-name> <repo-name>",
	Short: "Remove a backup repo from a metadata store",
	Long: `Remove a backup repo.

By default only the repo row (and its dependent backup_records rows) is deleted
— the archive files in the destination (manifest.yaml + payloads) remain on
disk / S3 and can be recovered by re-adding the repo with the same name.

With --purge-archives, the archive files are ALSO deleted from the destination.
This is irreversible. On partial failure (some archives couldn't be deleted),
the server preserves the repo row and surfaces the failed record IDs.

Examples:
  # Default: config-only delete (archives retained)
  dfsctl store metadata fast-meta repo remove daily-s3

  # Also delete destination artifacts
  dfsctl store metadata fast-meta repo remove daily-s3 --purge-archives --force`,
	Args: cobra.ExactArgs(2),
	RunE: runRemove,
}

func init() {
	removeCmd.Flags().BoolVar(&removePurgeArchives, "purge-archives", false, "Also delete archive files in the destination (irreversible)")
	removeCmd.Flags().BoolVarP(&removeForce, "force", "f", false, "Skip confirmation prompt")

	Cmd.AddCommand(removeCmd)
}

func runRemove(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}
	return doRemove(os.Stdout, client, args[0], args[1], removePurgeArchives, removeForce)
}

// doRemove is the testable core: it takes a client + writer so unit tests
// can inject an httptest-backed apiclient and capture stdout. runRemove is
// a thin wrapper that resolves the production client + stdout.
func doRemove(out io.Writer, client *apiclient.Client, storeName, repoName string, purgeArchives, force bool) error {
	confirmationLabel := fmt.Sprintf("Backup repo '%s'", repoName)
	if purgeArchives {
		confirmationLabel += " (WILL ALSO DELETE ARCHIVE FILES)"
	}

	return cmdutil.RunDeleteWithConfirmation(confirmationLabel, repoName, force, func() error {
		if err := client.DeleteBackupRepo(storeName, repoName, purgeArchives); err != nil {
			return fmt.Errorf("failed to delete backup repo: %w", err)
		}
		if purgeArchives {
			_, _ = fmt.Fprintf(out, "Backup repo '%s' removed (archive files purged).\n", repoName)
		} else {
			_, _ = fmt.Fprintf(out, "Backup repo '%s' removed (archive files retained).\n", repoName)
		}
		return nil
	})
}
