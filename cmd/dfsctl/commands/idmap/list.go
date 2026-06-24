package idmap

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var listProvider string

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List identity mappings",
	Long: `List identity mappings registered on the DittoFS server. Each row shows the
provider name, the external principal, and the local DittoFS username it maps to.
Use --provider to filter the list to a single identity provider, and -o json or
-o yaml for machine-readable output.

Examples:
  # List all identity mappings
  dfsctl idmap list

  # Show only Kerberos mappings
  dfsctl idmap list --provider kerberos

  # Show only AD mappings as JSON
  dfsctl idmap list --provider ad -o json

  # Output all mappings as YAML
  dfsctl idmap list -o yaml`,
	RunE: runList,
}

func init() {
	listCmd.Flags().StringVar(&listProvider, "provider", "", "Filter by identity provider (e.g., kerberos, oidc, ad)")
}

// MappingList is a list of identity mappings for table rendering.
type MappingList []apiclient.IdentityMapping

// Headers implements TableRenderer.
func (ml MappingList) Headers() []string {
	return []string{"PROVIDER", "PRINCIPAL", "USERNAME", "CREATED"}
}

// Rows implements TableRenderer.
func (ml MappingList) Rows() [][]string {
	rows := make([][]string, 0, len(ml))
	for _, m := range ml {
		created := cmdutil.EmptyOr(m.CreatedAt, "-")
		rows = append(rows, []string{m.ProviderName, m.Principal, m.Username, created})
	}
	return rows
}

func runList(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	mappings, err := client.ListIdentityMappings(listProvider)
	if err != nil {
		return fmt.Errorf("failed to list identity mappings: %w", err)
	}

	return cmdutil.PrintOutput(os.Stdout, mappings, len(mappings) == 0, "No identity mappings found.", MappingList(mappings))
}
