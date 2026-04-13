package idmap

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	addProvider  string
	addPrincipal string
	addUsername  string
)

var addCmd = &cobra.Command{
	Use:   "add",
	Short: "Add an identity mapping",
	Long: `Add a new identity mapping from an external identity to a DittoFS user.

Examples:
  # Map a Kerberos principal to a local user
  dfsctl idmap add --principal alice@EXAMPLE.COM --username alice

  # Map with explicit provider
  dfsctl idmap add --provider kerberos --principal admin@CORP.COM --username alice

  # Map a numeric UID principal
  dfsctl idmap add --principal 1000@localdomain --username bob`,
	RunE: runAdd,
}

func init() {
	addCmd.Flags().StringVar(&addProvider, "provider", "kerberos", "Identity provider (e.g., kerberos, oidc, ad)")
	addCmd.Flags().StringVar(&addPrincipal, "principal", "", "External identity (e.g., alice@EXAMPLE.COM)")
	addCmd.Flags().StringVar(&addUsername, "username", "", "DittoFS username")
	_ = addCmd.MarkFlagRequired("principal")
	_ = addCmd.MarkFlagRequired("username")
}

func runAdd(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	mapping, err := client.CreateIdentityMapping(addProvider, addPrincipal, addUsername)
	if err != nil {
		return fmt.Errorf("failed to create identity mapping: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, mapping,
		fmt.Sprintf("Identity mapping created: %s -> %s", mapping.Principal, mapping.Username))
}
