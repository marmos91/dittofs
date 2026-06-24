package trash

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
)

// emptyCmd purges every recycled root from a share's recycle bin.
var emptyCmd = &cobra.Command{
	Use:   "empty <share>",
	Short: "Empty a share's recycle bin",
	Long: `Permanently remove every entry from a share's recycle bin.

This operation is irreversible — all recycled files and directories are deleted from the server. A confirmation prompt is shown by default; use --force to skip it in non-interactive scripts.

Examples:
  # Empty the recycle bin with an interactive confirmation prompt
  dfsctl trash empty myshare

  # Empty non-interactively (e.g. in a cron job)
  dfsctl trash empty myshare --force`,
	Args: cobra.ExactArgs(1),
	RunE: runTrashEmpty,
}

func init() {
	emptyCmd.Flags().Bool("force", false, "Force empty, skipping server-side safety checks")
}

func runTrashEmpty(cmd *cobra.Command, args []string) error {
	share := args[0]
	force, _ := cmd.Flags().GetBool("force")

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	removed, err := client.TrashEmpty(share, force)
	if err != nil {
		return fmt.Errorf("failed to empty trash: %w", err)
	}

	cmdutil.PrintSuccess(fmt.Sprintf("Removed %d item(s)", removed))
	return nil
}
