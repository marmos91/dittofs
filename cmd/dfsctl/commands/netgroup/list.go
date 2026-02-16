package netgroup

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all netgroups",
	Long: `List all netgroups on the DittoFS server.

Examples:
  # List netgroups as table
  dfsctl netgroup list

  # List as JSON
  dfsctl netgroup list -o json`,
	RunE: runList,
}

// NetgroupList is a list of netgroups for table rendering.
type NetgroupList []*apiclient.Netgroup

// Headers implements TableRenderer.
func (nl NetgroupList) Headers() []string {
	return []string{"NAME", "MEMBERS", "CREATED"}
}

// Rows implements TableRenderer.
func (nl NetgroupList) Rows() [][]string {
	rows := make([][]string, 0, len(nl))
	for _, n := range nl {
		memberCount := fmt.Sprintf("%d", len(n.Members))
		created := n.CreatedAt.Format("2006-01-02 15:04:05")
		rows = append(rows, []string{n.Name, memberCount, created})
	}
	return rows
}

func runList(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	netgroups, err := client.ListNetgroups()
	if err != nil {
		return fmt.Errorf("failed to list netgroups: %w", err)
	}

	return cmdutil.PrintOutput(os.Stdout, netgroups, len(netgroups) == 0, "No netgroups found.", NetgroupList(netgroups))
}
