package idmap

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	addPrincipal string
	addUsername  string
)

var addCmd = &cobra.Command{
	Use:   "add",
	Short: "Add an identity mapping",
	Long: `Add a new identity mapping from an NFSv4 principal to a control plane user.

Examples:
  # Map a Kerberos principal to a local user
  dittofsctl idmap add --principal alice@EXAMPLE.COM --username alice

  # Map a numeric UID principal
  dittofsctl idmap add --principal 1000@localdomain --username bob`,
	RunE: runAdd,
}

func init() {
	addCmd.Flags().StringVar(&addPrincipal, "principal", "", "NFSv4 principal (e.g., alice@EXAMPLE.COM)")
	addCmd.Flags().StringVar(&addUsername, "username", "", "Control plane username")
	_ = addCmd.MarkFlagRequired("principal")
	_ = addCmd.MarkFlagRequired("username")
}

func runAdd(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	mapping, err := client.CreateIdentityMapping(addPrincipal, addUsername)
	if err != nil {
		return fmt.Errorf("failed to create identity mapping: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, mapping,
		fmt.Sprintf("Identity mapping created: %s -> %s", mapping.Principal, mapping.Username))
}
