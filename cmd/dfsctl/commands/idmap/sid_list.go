package idmap

import (
	"fmt"
	"os"
	"strconv"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var sidListCmd = &cobra.Command{
	Use:   "list",
	Short: "List foreign-SID UID/GID allocations",
	Long: `List durable foreign-SID to Unix UID/GID allocations on the DittoFS server.

Examples:
  # List all foreign-SID allocations
  dfsctl idmap sid list

  # List as JSON
  dfsctl idmap sid list -o json`,
	RunE: runSidList,
}

// SIDMappingList is a list of SID mappings for table rendering.
type SIDMappingList []apiclient.SIDMapping

// Headers implements TableRenderer.
func (sl SIDMappingList) Headers() []string {
	return []string{"SID", "KIND", "UNIX_ID", "DISPLAY_NAME", "CREATED"}
}

// Rows implements TableRenderer.
func (sl SIDMappingList) Rows() [][]string {
	rows := make([][]string, 0, len(sl))
	for _, m := range sl {
		kind := "user"
		if m.IsGroup {
			kind = "group"
		}
		created := cmdutil.EmptyOr(m.CreatedAt, "-")
		display := cmdutil.EmptyOr(m.DisplayName, "-")
		rows = append(rows, []string{
			m.SID,
			kind,
			strconv.FormatUint(uint64(m.UnixID), 10),
			display,
			created,
		})
	}
	return rows
}

func runSidList(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	mappings, err := client.ListSIDMappings()
	if err != nil {
		return fmt.Errorf("failed to list SID mappings: %w", err)
	}

	return cmdutil.PrintOutput(os.Stdout, mappings, len(mappings) == 0, "No SID mappings found.", SIDMappingList(mappings))
}
