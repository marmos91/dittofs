package netgroup

import (
	"fmt"
	"strings"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var removeForce bool

var removeCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a netgroup",
	Long: `Remove a netgroup from the DittoFS server. This action is irreversible.
If the netgroup is still referenced by one or more shares, the deletion fails
with a conflict error that lists the affected shares — remove those references
first. You will be prompted for confirmation unless --force is specified.

Examples:
  # Remove a netgroup (prompts for confirmation)
  dfsctl netgroup remove office-network

  # Remove a netgroup non-interactively (for scripts and automation)
  dfsctl netgroup remove office-network --force`,
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

	return cmdutil.RunDeleteWithConfirmation("Netgroup", name, removeForce, func() error {
		if err := client.RemoveNetgroup(name); err != nil {
			// Check for conflict (in-use by shares)
			if apiErr, ok := err.(*apiclient.APIError); ok && apiErr.IsConflict() {
				msg := fmt.Sprintf("failed to remove netgroup: %s", apiErr.Error())
				if apiErr.Hint != "" {
					msg += fmt.Sprintf("\n  Hint: %s", strings.TrimSpace(apiErr.Hint))
				}
				return fmt.Errorf("%s", msg)
			}
			return fmt.Errorf("failed to remove netgroup: %w", err)
		}
		return nil
	})
}
