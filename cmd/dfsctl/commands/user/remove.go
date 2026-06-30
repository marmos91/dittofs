package user

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var removeForce bool

var removeCmd = &cobra.Command{
	Use:   "remove <username|id>",
	Short: "Remove a user",
	Long: `Remove a user from the DittoFS server. Accepts either the username or
the user's full ID (see 'dfsctl user list'). This action is irreversible:
the account and its authentication tokens are permanently removed, though
files the user owns are not deleted. You will be prompted for confirmation
unless --force is specified.

Examples:
  # Remove a user (prompts for confirmation)
  dfsctl user remove alice

  # Remove a user non-interactively (for scripts and automation)
  dfsctl user remove alice --force`,
	Args: cobra.ExactArgs(1),
	RunE: runRemove,
}

func init() {
	removeCmd.Flags().BoolVarP(&removeForce, "force", "f", false, "Skip confirmation prompt")
}

func runRemove(cmd *cobra.Command, args []string) error {
	username := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	return cmdutil.RunDeleteWithConfirmation("User", username, removeForce, func() error {
		if err := client.RemoveUser(username); err != nil {
			return fmt.Errorf("failed to remove user: %w", err)
		}
		return nil
	})
}
