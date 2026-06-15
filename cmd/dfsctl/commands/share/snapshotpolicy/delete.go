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
	Long: `Delete a share's snapshot policy. Existing snapshots are not removed;
only the schedule and automatic pruning stop.`,
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
