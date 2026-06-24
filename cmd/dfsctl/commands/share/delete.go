package share

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var deleteForce bool

var deleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a share",
	Long: `Permanently delete a share from the DittoFS server.

Deleting a share removes its configuration from the control plane. The
underlying block and metadata stores are NOT deleted — only the share record
that ties them together. This operation is irreversible: you will be prompted
for confirmation unless --force is specified. Disable the share first
('dfsctl share disable') if you want to drain active clients before deleting.

Examples:
  # Delete a share, prompted for confirmation
  dfsctl share delete /archive

  # Delete without a confirmation prompt (useful in scripts)
  dfsctl share delete /archive --force

  # Drain clients first, then delete without prompting
  dfsctl share disable /archive && dfsctl share delete /archive --force`,
	Args: cobra.ExactArgs(1),
	RunE: runDelete,
}

func init() {
	deleteCmd.Flags().BoolVarP(&deleteForce, "force", "f", false, "Skip confirmation prompt")
}

func runDelete(cmd *cobra.Command, args []string) error {
	name := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	return cmdutil.RunDeleteWithConfirmation("Share", name, deleteForce, func() error {
		if err := client.DeleteShare(name); err != nil {
			return fmt.Errorf("failed to delete share: %w", err)
		}
		return nil
	})
}
