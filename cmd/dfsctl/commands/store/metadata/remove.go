package metadata

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var removeForce bool

var removeCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a metadata store",
	Long: `Remove a metadata store from the DittoFS server.

Warning: This may fail if the store is in use by any shares.

Examples:
  # Remove with confirmation
  dfsctl store metadata remove fast-meta

  # Remove without confirmation
  dfsctl store metadata remove fast-meta --force`,
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

	return cmdutil.RunDeleteWithConfirmation("Metadata store", name, removeForce, func() error {
		if err := client.DeleteMetadataStore(name); err != nil {
			return fmt.Errorf("failed to remove metadata store: %w", err)
		}
		return nil
	})
}
