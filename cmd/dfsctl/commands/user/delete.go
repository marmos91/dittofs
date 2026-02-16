package user

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var deleteForce bool

var deleteCmd = &cobra.Command{
	Use:   "delete <username>",
	Short: "Delete a user",
	Long: `Delete a user from the DittoFS server.

This action is irreversible. You will be prompted for confirmation
unless --force is specified.

Examples:
  # Delete user with confirmation
  dfsctl user delete alice

  # Delete user without confirmation
  dfsctl user delete alice --force`,
	Args: cobra.ExactArgs(1),
	RunE: runDelete,
}

func init() {
	deleteCmd.Flags().BoolVarP(&deleteForce, "force", "f", false, "Skip confirmation prompt")
}

func runDelete(cmd *cobra.Command, args []string) error {
	username := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	return cmdutil.RunDeleteWithConfirmation("User", username, deleteForce, func() error {
		if err := client.DeleteUser(username); err != nil {
			return fmt.Errorf("failed to delete user: %w", err)
		}
		return nil
	})
}
