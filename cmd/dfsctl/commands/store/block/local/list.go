package local

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List local block stores",
	Long: `List all local block stores on the DittoFS server.

Examples:
  # List as table
  dfsctl store block local list

  # List as JSON
  dfsctl store block local list -o json`,
	RunE: runList,
}

// StoreList is a list of block stores for table rendering.
type StoreList []apiclient.BlockStore

// Headers implements TableRenderer.
func (sl StoreList) Headers() []string {
	return []string{"NAME", "TYPE", "CONFIG"}
}

// Rows implements TableRenderer.
func (sl StoreList) Rows() [][]string {
	rows := make([][]string, 0, len(sl))
	for _, s := range sl {
		configStr := "-"
		if len(s.Config) > 0 && string(s.Config) != "null" {
			configStr = string(s.Config)
		}
		rows = append(rows, []string{s.Name, s.Type, configStr})
	}
	return rows
}

func runList(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	stores, err := client.ListBlockStores("local")
	if err != nil {
		return fmt.Errorf("failed to list local block stores: %w", err)
	}

	return cmdutil.PrintOutput(os.Stdout, stores, len(stores) == 0, "No local block stores found.", StoreList(stores))
}
