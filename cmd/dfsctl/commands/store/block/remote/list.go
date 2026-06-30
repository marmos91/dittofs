package remote

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List remote block stores",
	Long: `List all remote block stores on the DittoFS server.

Shows the name, ID, type (s3 or memory), and configuration of each registered
remote block store. Other sub-commands accept either form, so this is where you
find both. Use it to confirm which stores exist before adding, editing, or
running health checks against one, or to map the store IDs emitted by
'share show -o json' back to a store name ('share show' table output already
resolves them to names).

Examples:
  # List as table
  dfsctl store block remote list

  # List as JSON
  dfsctl store block remote list -o json

  # List as YAML
  dfsctl store block remote list -o yaml`,
	RunE: runList,
}

// StoreList is a list of block stores for table rendering.
type StoreList []apiclient.BlockStore

// Headers implements TableRenderer.
func (sl StoreList) Headers() []string {
	return []string{"NAME", "ID", "TYPE", "CONFIG"}
}

// Rows implements TableRenderer.
func (sl StoreList) Rows() [][]string {
	rows := make([][]string, 0, len(sl))
	for _, s := range sl {
		configStr := "-"
		if len(s.Config) > 0 && string(s.Config) != "null" {
			configStr = string(s.Config)
		}
		rows = append(rows, []string{s.Name, s.ID, s.Type, configStr})
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
		return fmt.Errorf("failed to list remote block stores: %w", err)
	}

	return cmdutil.PrintOutput(os.Stdout, stores, len(stores) == 0, "No remote block stores found.", StoreList(stores))
}
