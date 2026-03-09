package payload

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List block stores (remote)",
	Long: `List all remote block stores on the DittoFS server.

Note: This command is deprecated. Use 'dfsctl store block remote list' instead.

Examples:
  # List as table
  dfsctl store payload list

  # List as JSON
  dfsctl store payload list -o json`,
	RunE: runList,
}

// StoreList is a list of block stores for table rendering.
type StoreList []apiclient.BlockStore

// Headers implements TableRenderer.
func (sl StoreList) Headers() []string {
	return []string{"NAME", "KIND", "TYPE"}
}

// Rows implements TableRenderer.
func (sl StoreList) Rows() [][]string {
	rows := make([][]string, 0, len(sl))
	for _, s := range sl {
		rows = append(rows, []string{s.Name, s.Kind, s.Type})
	}
	return rows
}

func runList(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	stores, err := client.ListBlockStores("remote")
	if err != nil {
		return fmt.Errorf("failed to list block stores: %w", err)
	}

	return cmdutil.PrintOutput(os.Stdout, stores, len(stores) == 0, "No block stores found.", StoreList(stores))
}
