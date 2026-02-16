package idmap

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	removePrincipal string
	removeForce     bool
)

var removeCmd = &cobra.Command{
	Use:   "remove",
	Short: "Remove an identity mapping",
	Long: `Remove an identity mapping by principal.

This action is irreversible. You will be prompted for confirmation
unless --force is specified.

Examples:
  # Remove with confirmation
  dittofsctl idmap remove --principal alice@EXAMPLE.COM

  # Remove without confirmation
  dittofsctl idmap remove --principal alice@EXAMPLE.COM --force`,
	RunE: runRemove,
}

func init() {
	removeCmd.Flags().StringVar(&removePrincipal, "principal", "", "NFSv4 principal to remove")
	removeCmd.Flags().BoolVarP(&removeForce, "force", "f", false, "Skip confirmation prompt")
	_ = removeCmd.MarkFlagRequired("principal")
}

func runRemove(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	return cmdutil.RunDeleteWithConfirmation("Identity mapping", removePrincipal, removeForce, func() error {
		if err := client.DeleteIdentityMapping(removePrincipal); err != nil {
			return fmt.Errorf("failed to delete identity mapping: %w", err)
		}
		return nil
	})
}
