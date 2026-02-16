package payload

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var removeForce bool

var removeCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a payload store",
	Long: `Remove a payload store from the DittoFS server.

Warning: This may fail if the store is in use by any shares.

Examples:
  # Remove with confirmation
  dfsctl store payload remove fast-content

  # Remove without confirmation
  dfsctl store payload remove fast-content --force`,
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

	return cmdutil.RunDeleteWithConfirmation("Payload store", name, removeForce, func() error {
		if err := client.DeletePayloadStore(name); err != nil {
			return fmt.Errorf("failed to remove payload store: %w", err)
		}
		return nil
	})
}
