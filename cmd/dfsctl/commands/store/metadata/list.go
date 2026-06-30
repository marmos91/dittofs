package metadata

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List metadata stores",
	Long: `List all metadata stores on the DittoFS server.

Displays the name, ID, and type of every registered metadata store. Use this to
confirm which stores are configured before adding or removing one, to identify
the store name needed by other sub-commands such as health, or to match the
store IDs shown by 'share show' back to a store name.

Examples:
  # List as table
  dfsctl store metadata list

  # List as JSON
  dfsctl store metadata list -o json

  # List as YAML
  dfsctl store metadata list -o yaml`,
	RunE: runList,
}

// StoreList is a list of metadata stores for table rendering.
type StoreList []apiclient.MetadataStore

// Headers implements TableRenderer.
func (sl StoreList) Headers() []string {
	return []string{"NAME", "ID", "TYPE"}
}

// Rows implements TableRenderer.
func (sl StoreList) Rows() [][]string {
	rows := make([][]string, 0, len(sl))
	for _, s := range sl {
		rows = append(rows, []string{s.Name, s.ID, s.Type})
	}
	return rows
}

func runList(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	stores, err := client.ListMetadataStores()
	if err != nil {
		return fmt.Errorf("failed to list metadata stores: %w", err)
	}

	return cmdutil.PrintOutput(os.Stdout, stores, len(stores) == 0, "No metadata stores found.", StoreList(stores))
}
