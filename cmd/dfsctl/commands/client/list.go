package client

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var protocolFlag string

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List connected clients",
	Long: `List all clients currently connected to the DittoFS server.

Each row shows the client ID, protocol (NFS or SMB), remote address, and how long the client has been connected. Use --protocol to narrow the output.

Examples:
  # List all connected clients
  dfsctl client list

  # Show only NFS clients
  dfsctl client list --protocol nfs

  # Get the client list as JSON
  dfsctl client list -o json`,
	RunE: runList,
}

func init() {
	listCmd.Flags().StringVar(&protocolFlag, "protocol", "", "Filter by protocol (nfs, smb)")
}

// ClientList is a list of clients for table rendering.
type ClientList []apiclient.ClientInfo

// Headers implements TableRenderer.
func (cl ClientList) Headers() []string {
	return []string{"CLIENT_ID", "PROTOCOL", "ADDRESS", "CONNECTED"}
}

// Rows implements TableRenderer.
func (cl ClientList) Rows() [][]string {
	rows := make([][]string, 0, len(cl))
	for _, c := range cl {
		rows = append(rows, []string{
			c.ClientID,
			strings.ToUpper(c.Protocol),
			c.Address,
			time.Since(c.ConnectedAt).Truncate(time.Second).String(),
		})
	}
	return rows
}

func runList(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	var opts []apiclient.ListClientsOption
	if protocolFlag != "" {
		opts = append(opts, apiclient.WithProtocol(protocolFlag))
	}

	clients, err := client.ListClients(opts...)
	if err != nil {
		return fmt.Errorf("failed to list clients: %w", err)
	}

	return cmdutil.PrintOutput(os.Stdout, clients, len(clients) == 0, "No connected clients.", ClientList(clients))
}
