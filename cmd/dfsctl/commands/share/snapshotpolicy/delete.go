package snapshotpolicy

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var deleteYes bool

var deleteCmd = &cobra.Command{
	Use:   "delete <share>",
	Short: "Delete a share's snapshot policy",
	Long: `Delete the snapshot policy for a share.

Existing snapshots are not removed; only the schedule and automatic pruning
stop. After deletion, no new scheduled snapshots will be created and old
scheduled snapshots will no longer be pruned. Use 'snapshot-policy set' to
recreate a policy at any time.

Examples:
  # Delete the policy, with a confirmation prompt
  dfsctl share snapshot-policy delete /archive

  # Delete without a confirmation prompt (useful in scripts)
  dfsctl share snapshot-policy delete /archive --yes`,
	Args: cobra.ExactArgs(1),
	RunE: runDelete,
}

func init() {
	deleteCmd.Flags().BoolVar(&deleteYes, "yes", false, "Skip confirmation prompt")
}

func runDelete(cmd *cobra.Command, args []string) error {
	share := args[0]

	client, err := getClient()
	if err != nil {
		return err
	}

	prompt := fmt.Sprintf("Delete snapshot policy for share %s? Automatic snapshots will stop.", share)
	ok, err := cmdutil.ConfirmDestructive(prompt, deleteYes)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("Aborted.")
		return nil
	}

	if err := client.DeleteSnapshotPolicy(share); err != nil {
		return fmt.Errorf("failed to delete snapshot policy: %w", err)
	}
	fmt.Printf("Snapshot policy for share %s deleted.\n", share)
	return nil
}
