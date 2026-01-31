package share

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all shares",
	Long: `List all shares on the DittoFS server.

Examples:
  # List shares as table
  dittofsctl share list

  # List as JSON
  dittofsctl share list -o json

  # List as YAML
  dittofsctl share list -o yaml`,
	RunE: runList,
}

// ShareList is a list of shares for table rendering.
type ShareList []apiclient.Share

// Headers implements TableRenderer.
func (sl ShareList) Headers() []string {
	return []string{"NAME", "METADATA STORE", "PAYLOAD STORE", "DEFAULT PERMISSION"}
}

// Rows implements TableRenderer.
func (sl ShareList) Rows() [][]string {
	rows := make([][]string, 0, len(sl))
	for _, s := range sl {
		rows = append(rows, []string{s.Name, s.MetadataStoreID, s.PayloadStoreID, s.DefaultPermission})
	}
	return rows
}

func runList(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	shares, err := client.ListShares()
	if err != nil {
		return fmt.Errorf("failed to list shares: %w", err)
	}

	return cmdutil.PrintOutput(os.Stdout, shares, len(shares) == 0, "No shares found.", ShareList(shares))
}
