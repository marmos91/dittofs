package share

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all shares",
	Long: `List all shares on the DittoFS server.

Examples:
  # List shares as table
  dfsctl share list

  # List as JSON
  dfsctl share list -o json

  # List as YAML
  dfsctl share list -o yaml`,
	RunE: runList,
}

// shareRow holds resolved share info for table display.
type shareRow struct {
	Name              string `json:"name"`
	MetadataStore     string `json:"metadata_store"`
	PayloadStore      string `json:"payload_store"`
	DefaultPermission string `json:"default_permission"`
}

// ShareList is a list of shares for table rendering.
type ShareList []shareRow

// Headers implements TableRenderer.
func (sl ShareList) Headers() []string {
	return []string{"NAME", "METADATA STORE", "PAYLOAD STORE", "DEFAULT PERMISSION"}
}

// Rows implements TableRenderer.
func (sl ShareList) Rows() [][]string {
	rows := make([][]string, 0, len(sl))
	for _, s := range sl {
		rows = append(rows, []string{s.Name, s.MetadataStore, s.PayloadStore, s.DefaultPermission})
	}
	return rows
}

// buildStoreNameMaps fetches metadata and payload stores and builds ID->name lookup maps.
func buildStoreNameMaps(client *apiclient.Client) (metaMap, payloadMap map[string]string) {
	metaMap = make(map[string]string)
	payloadMap = make(map[string]string)

	if metaStores, err := client.ListMetadataStores(); err == nil {
		for _, s := range metaStores {
			metaMap[s.ID] = s.Name
		}
	}

	if payloadStores, err := client.ListPayloadStores(); err == nil {
		for _, s := range payloadStores {
			payloadMap[s.ID] = s.Name
		}
	}

	return metaMap, payloadMap
}

// resolveStoreName returns the human-readable name for a store ID,
// falling back to the raw ID if not found in the lookup map.
func resolveStoreName(nameMap map[string]string, id string) string {
	if name := nameMap[id]; name != "" {
		return name
	}
	return id
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

	metaNames, payloadNames := buildStoreNameMaps(client)

	rows := make(ShareList, 0, len(shares))
	for _, s := range shares {
		rows = append(rows, shareRow{
			Name:              s.Name,
			MetadataStore:     resolveStoreName(metaNames, s.MetadataStoreID),
			PayloadStore:      resolveStoreName(payloadNames, s.PayloadStoreID),
			DefaultPermission: s.DefaultPermission,
		})
	}

	return cmdutil.PrintOutput(os.Stdout, rows, len(rows) == 0, "No shares found.", rows)
}
