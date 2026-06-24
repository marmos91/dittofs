package trash

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// restoreCmd moves a recycled root back out of the recycle bin.
var restoreCmd = &cobra.Command{
	Use:   "restore <share> <bin-path>",
	Short: "Restore a recycled file or directory",
	Long: `Restore the recycled entry at <bin-path> back into the share.

Without --to the entry is moved back to the path it occupied before deletion. If that location is now taken, use --to to restore it elsewhere. The bin-path argument is the value shown in the PATH column of 'trash list'.

Examples:
  # Restore a file to its original location
  dfsctl trash restore myshare "#recycle/2026-06-01/report.txt"

  # Restore to a different path when the original location is occupied
  dfsctl trash restore myshare "#recycle/2026-06-01/report.txt" --to /archive/report.txt`,
	Args: cobra.ExactArgs(2),
	RunE: runTrashRestore,
}

func init() {
	restoreCmd.Flags().String("to", "", "Restore to this share-relative path instead of the original location")
}

func runTrashRestore(cmd *cobra.Command, args []string) error {
	share := args[0]
	binPath := args[1]
	to, _ := cmd.Flags().GetString("to")

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	if err := client.TrashRestore(share, binPath, to); err != nil {
		var apiErr *apiclient.APIError
		if errors.As(err, &apiErr) && apiErr.IsConflict() {
			return fmt.Errorf("cannot restore %q: destination exists; use --to to restore elsewhere", binPath)
		}
		return fmt.Errorf("failed to restore %q: %w", binPath, err)
	}

	cmdutil.PrintSuccess(fmt.Sprintf("Restored %s", binPath))
	return nil
}
