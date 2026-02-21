package client

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List connected NFS clients",
	Long: `List all connected NFS clients on the DittoFS server.

Displays both NFSv4.0 and NFSv4.1 clients with their version,
address, lease status, and implementation info.

Examples:
  # List as table
  dfsctl client list

  # List as JSON
  dfsctl client list -o json

  # List as YAML
  dfsctl client list -o yaml`,
	RunE: runList,
}

// ClientList is a list of clients for table rendering.
type ClientList []apiclient.ClientInfo

// Headers implements TableRenderer.
func (cl ClientList) Headers() []string {
	return []string{"CLIENT_ID", "VERSION", "ADDRESS", "LEASE", "CONFIRMED", "IMPL_NAME"}
}

// Rows implements TableRenderer.
func (cl ClientList) Rows() [][]string {
	rows := make([][]string, 0, len(cl))
	for _, c := range cl {
		// Truncate client_id to last 8 hex chars for readability
		shortID := c.ClientID
		if len(shortID) > 8 {
			shortID = "..." + shortID[len(shortID)-8:]
		}
		rows = append(rows, []string{
			shortID,
			c.NFSVersion,
			c.Address,
			c.LeaseStatus,
			cmdutil.BoolToYesNo(c.Confirmed),
			cmdutil.EmptyOr(c.ImplName, "-"),
		})
	}
	return rows
}

func runList(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	clients, err := client.ListClients()
	if err != nil {
		return fmt.Errorf("failed to list clients: %w", err)
	}

	return cmdutil.PrintOutput(os.Stdout, clients, len(clients) == 0, "No connected NFS clients.", ClientList(clients))
}
