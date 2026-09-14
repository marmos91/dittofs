package block

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var removeForce bool

var removeCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a block store",
	Long: `Remove a block store from the DittoFS server.

The server refuses removal if any share currently references the store.
Detach the store from all shares first, then remove it. No objects are
deleted from the remote bucket by this command. You will be prompted for
confirmation unless --force is specified.

Examples:
  # Remove with confirmation prompt
  dfsctl store block remove s3-store

  # Remove without confirmation
  dfsctl store block remove s3-store --force

  # Verify the store is gone afterward
  dfsctl store block list`,
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

	return cmdutil.RunDeleteWithConfirmation("Block store", name, removeForce, func() error {
		if err := client.RemoveBlockStore(name); err != nil {
			return fmt.Errorf("failed to remove block store: %w", err)
		}
		return nil
	})
}
