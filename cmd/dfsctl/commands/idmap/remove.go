package idmap

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	removeProvider  string
	removePrincipal string
	removeForce     bool
)

var removeCmd = &cobra.Command{
	Use:   "remove",
	Short: "Remove an identity mapping",
	Long: `Remove an identity mapping by provider and principal. Once removed, the
external principal will no longer be automatically resolved to the local user,
and connections authenticated with that principal will be rejected or treated
as anonymous. This action is irreversible. You will be prompted for
confirmation unless --force is specified.

Examples:
  # Remove a Kerberos mapping (prompts for confirmation)
  dfsctl idmap remove --principal alice@EXAMPLE.COM

  # Remove an AD mapping with explicit provider
  dfsctl idmap remove --provider ad --principal CORP\\alice

  # Remove a mapping non-interactively (for scripts)
  dfsctl idmap remove --principal alice@EXAMPLE.COM --force`,
	RunE: runRemove,
}

func init() {
	removeCmd.Flags().StringVar(&removeProvider, "provider", "kerberos", "Identity provider (e.g., kerberos, oidc, ad)")
	removeCmd.Flags().StringVar(&removePrincipal, "principal", "", "External identity to remove")
	removeCmd.Flags().BoolVarP(&removeForce, "force", "f", false, "Skip confirmation prompt")
	_ = removeCmd.MarkFlagRequired("principal")
}

func runRemove(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	return cmdutil.RunDeleteWithConfirmation("Identity mapping", removePrincipal, removeForce, func() error {
		if err := client.DeleteIdentityMapping(removeProvider, removePrincipal); err != nil {
			return fmt.Errorf("failed to remove identity mapping: %w", err)
		}
		return nil
	})
}
