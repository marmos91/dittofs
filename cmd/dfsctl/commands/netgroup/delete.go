package netgroup

import (
	"fmt"
	"strings"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var deleteForce bool

var deleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a netgroup",
	Long: `Delete a netgroup from the DittoFS server. This action is irreversible.
If the netgroup is still referenced by one or more shares, the deletion fails
with a conflict error that lists the affected shares — remove those references
first. You will be prompted for confirmation unless --force is specified.

Examples:
  # Delete a netgroup (prompts for confirmation)
  dfsctl netgroup delete office-network

  # Delete a netgroup non-interactively (for scripts and automation)
  dfsctl netgroup delete office-network --force`,
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

	return cmdutil.RunDeleteWithConfirmation("Netgroup", name, deleteForce, func() error {
		if err := client.DeleteNetgroup(name); err != nil {
			// Check for conflict (in-use by shares)
			if apiErr, ok := err.(*apiclient.APIError); ok && apiErr.IsConflict() {
				msg := fmt.Sprintf("failed to delete netgroup: %s", apiErr.Error())
				if apiErr.Hint != "" {
					msg += fmt.Sprintf("\n  Hint: %s", strings.TrimSpace(apiErr.Hint))
				}
				return fmt.Errorf("%s", msg)
			}
			return fmt.Errorf("failed to delete netgroup: %w", err)
		}
		return nil
	})
}
