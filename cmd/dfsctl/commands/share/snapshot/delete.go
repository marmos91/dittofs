package snapshot

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var deleteYes bool

var deleteCmd = &cobra.Command{
	Use:   "delete <share> <id>",
	Short: "Delete a snapshot",
	Long: `Delete a snapshot. This is irreversible.

Examples:
  # Delete with prompt
  dfsctl share snapshot delete /archive snap-abc123

  # Delete without prompt
  dfsctl share snapshot delete /archive snap-abc123 --yes`,
	Args: cobra.ExactArgs(2),
	RunE: runDelete,
}

func init() {
	deleteCmd.Flags().BoolVar(&deleteYes, "yes", false, "Skip confirmation prompt")
}

func runDelete(cmd *cobra.Command, args []string) error {
	share, id := args[0], args[1]

	prompt := fmt.Sprintf("Delete snapshot %s from share %s? This cannot be undone.", id, share)
	ok, err := cmdutil.ConfirmDestructive(prompt, deleteYes)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("Aborted.")
		return nil
	}

	client, err := getClient()
	if err != nil {
		return err
	}

	if err := client.DeleteSnapshot(share, id); err != nil {
		return fmt.Errorf("failed to delete snapshot: %w", err)
	}

	fmt.Printf("Snapshot %s deleted.\n", id)
	return nil
}
