package share

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/spf13/cobra"
)

var deleteForce bool

var deleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a share",
	Long: `Delete a share from the DittoFS server.

This action is irreversible. You will be prompted for confirmation
unless --force is specified.

Examples:
  # Delete share with confirmation
  dittofsctl share delete /archive

  # Delete share without confirmation
  dittofsctl share delete /archive --force`,
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

	return cmdutil.RunDeleteWithConfirmation("Share", name, deleteForce, func() error {
		if err := client.DeleteShare(name); err != nil {
			return fmt.Errorf("failed to delete share: %w", err)
		}
		return nil
	})
}
