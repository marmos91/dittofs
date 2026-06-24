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

var (
	protocolFlag string
	shareFlag    string
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List connected clients",
	Long: `List all clients currently connected to the DittoFS server.

Each row shows the client ID, protocol (NFS or SMB), remote address, authenticated user, mounted shares, and how long the client has been connected. Use --protocol or --share to narrow the output.

Examples:
  # List all connected clients
  dfsctl client list

  # Show only NFS clients
  dfsctl client list --protocol nfs

  # Show only clients connected to a specific share
  dfsctl client list --share myshare

  # Get the client list as JSON
  dfsctl client list -o json`,
	RunE: runList,
}

func init() {
	listCmd.Flags().StringVar(&protocolFlag, "protocol", "", "Filter by protocol (nfs, smb)")
	listCmd.Flags().StringVar(&shareFlag, "share", "", "Filter by share name")
}

// ClientList is a list of clients for table rendering.
type ClientList []apiclient.ClientInfo

// Headers implements TableRenderer.
func (cl ClientList) Headers() []string {
	return []string{"CLIENT_ID", "PROTOCOL", "ADDRESS", "USER", "SHARES", "CONNECTED"}
}

// Rows implements TableRenderer.
func (cl ClientList) Rows() [][]string {
	rows := make([][]string, 0, len(cl))
	for _, c := range cl {
		shares := "-"
		if len(c.Shares) > 0 {
			shares = strings.Join(c.Shares, ", ")
		}
		connected := time.Since(c.ConnectedAt).Truncate(time.Second).String()
		rows = append(rows, []string{
			c.ClientID,
			strings.ToUpper(c.Protocol),
			c.Address,
			cmdutil.EmptyOr(c.User, "-"),
			shares,
			connected,
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
	if shareFlag != "" {
		opts = append(opts, apiclient.WithShare(shareFlag))
	}

	clients, err := client.ListClients(opts...)
	if err != nil {
		return fmt.Errorf("failed to list clients: %w", err)
	}

	return cmdutil.PrintOutput(os.Stdout, clients, len(clients) == 0, "No connected clients.", ClientList(clients))
}
