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

Warning: This will fail if the store is in use by any shares.

Examples:
  # Remove with confirmation
  dfsctl store block local remove fs-cache

  # Remove without confirmation
  dfsctl store block local remove fs-cache --force`,
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
		if err := client.DeleteBlockStore("local", name); err != nil {
			return fmt.Errorf("failed to remove local block store: %w", err)
		}
		return nil
	})
}
