package snapshotpolicy

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var removeYes bool

var removeCmd = &cobra.Command{
	Use:   "remove <share>",
	Short: "Remove a share's snapshot policy",
	Long: `Remove the snapshot policy for a share.

Existing snapshots are not removed; only the schedule and automatic pruning
stop. After removal, no new scheduled snapshots will be created and old
scheduled snapshots will no longer be pruned. Use 'snapshot-policy set' to
recreate a policy at any time.

Examples:
  # Remove the policy, with a confirmation prompt
  dfsctl share snapshot-policy remove /archive

  # Remove without a confirmation prompt (useful in scripts)
  dfsctl share snapshot-policy remove /archive --yes`,
	Args: cobra.ExactArgs(1),
	RunE: runRemove,
}

func init() {
	removeCmd.Flags().BoolVar(&removeYes, "yes", false, "Skip confirmation prompt")
}

func runRemove(cmd *cobra.Command, args []string) error {
	share := args[0]

	client, err := getClient()
	if err != nil {
		return err
	}

	prompt := fmt.Sprintf("Remove snapshot policy for share %s? Automatic snapshots will stop.", share)
	ok, err := cmdutil.ConfirmDestructive(prompt, removeYes)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("Aborted.")
		return nil
	}

	if err := client.RemoveSnapshotPolicy(share); err != nil {
		return fmt.Errorf("failed to remove snapshot policy: %w", err)
	}
	fmt.Printf("Snapshot policy for share %s removed.\n", share)
	return nil
}
