package payload

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List payload stores",
	Long: `List all payload stores on the DittoFS server.

Examples:
  # List as table
  dittofsctl store payload list

  # List as JSON
  dittofsctl store payload list -o json`,
	RunE: runList,
}

// StoreList is a list of payload stores for table rendering.
type StoreList []apiclient.PayloadStore

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

	stores, err := client.ListPayloadStores()
	if err != nil {
		return fmt.Errorf("failed to list payload stores: %w", err)
	}

	return cmdutil.PrintOutput(os.Stdout, stores, len(stores) == 0, "No payload stores found.", StoreList(stores))
}
