package snapshot

import (
	"errors"
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	restoreYes   bool
	restoreForce bool
)

var restoreCmd = &cobra.Command{
	Use:   "restore <share> <id>",
	Short: "Restore a snapshot into a (disabled) share",
	Long: `Restore a snapshot into a share.

The target share must be disabled first; the command refuses with a
hint when the share is still enabled. Restore is destructive: a safety
snapshot is taken before the reset and its ID is printed on success
(delete it once you have verified the restored share).

Examples:
  # Restore with prompt
  dfsctl share disable /archive
  dfsctl share snapshot restore /archive snap-abc123

  # Restore without prompt
  dfsctl share snapshot restore /archive snap-abc123 --yes

  # Restore a snapshot that is not remotely durable
  dfsctl share snapshot restore /archive snap-abc123 --yes --force`,
	Args: cobra.ExactArgs(2),
	RunE: runRestore,
}

func init() {
	restoreCmd.Flags().BoolVar(&restoreYes, "yes", false, "Skip confirmation prompt")
	restoreCmd.Flags().BoolVar(&restoreForce, "force", false, "Allow restoring a snapshot that is not remotely durable")
}

func runRestore(cmd *cobra.Command, args []string) error {
	share, id := args[0], args[1]

	client, err := getClient()
	if err != nil {
		return err
	}

	// Pre-flight: refuse on enabled share with the exact hint string.
	s, err := client.GetShare(share)
	if err != nil {
		return fmt.Errorf("failed to look up share: %w", err)
	}
	if s.Enabled {
		fmt.Fprintf(os.Stderr, "share %s is enabled; run 'dfsctl share disable %s' first\n", share, share)
		return errors.New("share is enabled")
	}

	prompt := fmt.Sprintf("Restore snapshot %s into share %s? This will reset the share's data.", id, share)
	ok, err := cmdutil.ConfirmDestructive(prompt, restoreYes)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("Aborted.")
		return nil
	}

	resp, err := client.RestoreSnapshot(share, id, apiclient.RestoreSnapshotRequest{
		AllowNonDurable: restoreForce,
	})
	if err != nil {
		var apiErr *apiclient.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 412 {
			fmt.Fprintf(os.Stderr, "Snapshot %s is not remotely durable. Re-run with --force to restore anyway.\n", id)
			return errors.New("snapshot not durable")
		}
		return fmt.Errorf("failed to restore snapshot: %w", err)
	}

	fmt.Printf("Restored snapshot %s into share %s.\n", id, share)
	if resp.SafetySnapshotID != "" {
		fmt.Printf("Safety snap: %s (delete with 'dfsctl share snapshot delete %s %s' after verifying).\n",
			resp.SafetySnapshotID, share, resp.SafetySnapshotID)
	}
	return nil
}
