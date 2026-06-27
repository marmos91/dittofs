package group

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var removeForce bool

var removeCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a group",
	Long: `Remove a group from the DittoFS server. This action is irreversible:
the group record is permanently removed and any users that had it as their
primary group will lose that association. You will be prompted for
confirmation unless --force is specified.

Examples:
  # Remove a group (prompts for confirmation)
  dfsctl group remove editors

  # Remove a group non-interactively (for scripts and automation)
  dfsctl group remove editors --force`,
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

	return cmdutil.RunDeleteWithConfirmation("Group", name, removeForce, func() error {
		if err := client.RemoveGroup(name); err != nil {
			return fmt.Errorf("failed to remove group: %w", err)
		}
		return nil
	})
}
