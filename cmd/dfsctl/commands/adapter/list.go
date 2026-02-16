package adapter

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List protocol adapters",
	Long: `List all protocol adapters on the DittoFS server.

Examples:
  # List as table
  dfsctl adapter list

  # List as JSON
  dfsctl adapter list -o json`,
	RunE: runList,
}

// AdapterList is a list of adapters for table rendering.
type AdapterList []apiclient.Adapter

// Headers implements TableRenderer.
func (al AdapterList) Headers() []string {
	return []string{"TYPE", "PORT", "ENABLED"}
}

// Rows implements TableRenderer.
func (al AdapterList) Rows() [][]string {
	rows := make([][]string, 0, len(al))
	for _, a := range al {
		rows = append(rows, []string{a.Type, fmt.Sprintf("%d", a.Port), cmdutil.BoolToYesNo(a.Enabled)})
	}
	return rows
}

func runList(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	adapters, err := client.ListAdapters()
	if err != nil {
		return fmt.Errorf("failed to list adapters: %w", err)
	}

	return cmdutil.PrintOutput(os.Stdout, adapters, len(adapters) == 0, "No adapters found.", AdapterList(adapters))
}
