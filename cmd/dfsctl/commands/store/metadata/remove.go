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

The server refuses removal if any share currently references the store.
Detach the store from all shares first, then remove it. You will be prompted
for confirmation unless --force is specified.

Examples:
  # Remove with confirmation prompt
  dfsctl store metadata remove fast-meta

  # Remove without confirmation
  dfsctl store metadata remove fast-meta --force

  # Verify the store is gone afterward
  dfsctl store metadata list`,
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
