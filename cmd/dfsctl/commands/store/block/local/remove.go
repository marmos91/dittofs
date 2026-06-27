package local

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var removeForce bool

var removeCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a local block store",
	Long: `Remove a local block store from the DittoFS server.

The server refuses removal if any share currently references the store.
Detach the store from all shares first, then remove it. Data on disk is
not deleted by this command. You will be prompted for confirmation unless
--force is specified.

Examples:
  # Remove with confirmation prompt
  dfsctl store block local remove fs-cache

  # Remove without confirmation
  dfsctl store block local remove fs-cache --force

  # Verify the store is gone afterward
  dfsctl store block local list`,
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

	return cmdutil.RunDeleteWithConfirmation("Local block store", name, removeForce, func() error {
		if err := client.RemoveBlockStore("local", name); err != nil {
			return fmt.Errorf("failed to remove local block store: %w", err)
		}
		return nil
	})
}
