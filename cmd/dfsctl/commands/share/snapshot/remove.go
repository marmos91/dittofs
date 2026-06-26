package snapshot

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var removeYes bool

var removeCmd = &cobra.Command{
	Use:   "remove <share> <id>",
	Short: "Remove a snapshot",
	Long: `Remove a snapshot. This is irreversible.

Examples:
  # Remove with prompt
  dfsctl share snapshot remove /archive snap-abc123

  # Remove without prompt
  dfsctl share snapshot remove /archive snap-abc123 --yes`,
	Args: cobra.ExactArgs(2),
	RunE: runRemove,
}

func init() {
	removeCmd.Flags().BoolVar(&removeYes, "yes", false, "Skip confirmation prompt")
}

func runRemove(cmd *cobra.Command, args []string) error {
	share, id := args[0], args[1]

	client, err := getClient()
	if err != nil {
		return err
	}

	id, err = resolveSnapshotID(client, share, id)
	if err != nil {
		return err
	}

	prompt := fmt.Sprintf("Remove snapshot %s from share %s? This cannot be undone.", id, share)
	ok, err := cmdutil.ConfirmDestructive(prompt, removeYes)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("Aborted.")
		return nil
	}

	if err := client.DeleteSnapshot(share, id); err != nil {
		return fmt.Errorf("failed to remove snapshot: %w", err)
	}

	fmt.Printf("Snapshot %s removed.\n", id)
	return nil
}
