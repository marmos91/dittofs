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
	Long: `Delete a group from the DittoFS server. This action is irreversible:
the group record is permanently removed and any users that had it as their
primary group will lose that association. You will be prompted for
confirmation unless --force is specified.

Examples:
  # Delete a group (prompts for confirmation)
  dfsctl group delete editors

  # Delete a group non-interactively (for scripts and automation)
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
