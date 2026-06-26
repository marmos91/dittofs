package share

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var removeForce bool

var removeCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a share",
	Long: `Permanently remove a share from the DittoFS server.

Removing a share removes its configuration from the control plane. The
underlying block and metadata stores are NOT deleted — only the share record
that ties them together. This operation is irreversible: you will be prompted
for confirmation unless --force is specified. Disable the share first
('dfsctl share disable') if you want to drain active clients before removing.

Examples:
  # Remove a share, prompted for confirmation
  dfsctl share remove /archive

  # Remove without a confirmation prompt (useful in scripts)
  dfsctl share remove /archive --force

  # Drain clients first, then remove without prompting
  dfsctl share disable /archive && dfsctl share remove /archive --force`,
	Args: cobra.ExactArgs(1),
	RunE: runRemove,
}

func init() {
	removeCmd.Flags().BoolVarP(&removeForce, "force", "f", false, "Skip confirmation prompt")
}

func runRemove(cmd *cobra.Command, args []string) error {
	name := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	return cmdutil.RunDeleteWithConfirmation("Share", name, removeForce, func() error {
		if err := client.DeleteShare(name); err != nil {
			return fmt.Errorf("failed to remove share: %w", err)
		}
		return nil
	})
}
