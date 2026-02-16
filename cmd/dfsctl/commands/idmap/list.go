package idmap

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all identity mappings",
	Long: `List all identity mappings on the DittoFS server.

Examples:
  # List mappings as table
  dfsctl idmap list

  # List as JSON
  dfsctl idmap list -o json

  # List as YAML
  dfsctl idmap list -o yaml`,
	RunE: runList,
}

// MappingList is a list of identity mappings for table rendering.
type MappingList []apiclient.IdentityMapping

// Headers implements TableRenderer.
func (ml MappingList) Headers() []string {
	return []string{"PRINCIPAL", "USERNAME", "CREATED"}
}

// Rows implements TableRenderer.
func (ml MappingList) Rows() [][]string {
	rows := make([][]string, 0, len(ml))
	for _, m := range ml {
		created := cmdutil.EmptyOr(m.CreatedAt, "-")
		rows = append(rows, []string{m.Principal, m.Username, created})
	}
	return rows
}

func runList(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	mappings, err := client.ListIdentityMappings()
	if err != nil {
		return fmt.Errorf("failed to list identity mappings: %w", err)
	}

	return cmdutil.PrintOutput(os.Stdout, mappings, len(mappings) == 0, "No identity mappings found.", MappingList(mappings))
}
