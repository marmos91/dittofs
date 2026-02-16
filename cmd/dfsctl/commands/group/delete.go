package group

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var deleteForce bool

var deleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a group",
	Long: `Delete a group from the DittoFS server.

This action is irreversible. You will be prompted for confirmation
unless --force is specified.

Examples:
  # Delete group with confirmation
  dfsctl group delete editors

  # Delete group without confirmation
  dfsctl group delete editors --force`,
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

	return cmdutil.RunDeleteWithConfirmation("Group", name, deleteForce, func() error {
		if err := client.DeleteGroup(name); err != nil {
			return fmt.Errorf("failed to delete group: %w", err)
		}
		return nil
	})
}
