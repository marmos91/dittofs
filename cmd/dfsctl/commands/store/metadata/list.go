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

Examples:
  # List as table
  dfsctl store metadata list

  # List as JSON
  dfsctl store metadata list -o json`,
	RunE: runList,
}

// StoreList is a list of metadata stores for table rendering.
type StoreList []apiclient.MetadataStore

// Headers implements TableRenderer.
func (sl StoreList) Headers() []string {
	return []string{"NAME", "TYPE"}
}

// Rows implements TableRenderer.
func (sl StoreList) Rows() [][]string {
	rows := make([][]string, 0, len(sl))
	for _, s := range sl {
		rows = append(rows, []string{s.Name, s.Type})
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
