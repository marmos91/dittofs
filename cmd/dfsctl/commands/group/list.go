package group

import (
	"fmt"
	"os"
	"strings"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all groups",
	Long: `List all groups on the DittoFS server.

Examples:
  # List groups as table
  dfsctl group list

  # List as JSON
  dfsctl group list -o json

  # List as YAML
  dfsctl group list -o yaml`,
	RunE: runList,
}

// GroupList is a list of groups for table rendering.
type GroupList []apiclient.Group

// Headers implements TableRenderer.
func (gl GroupList) Headers() []string {
	return []string{"NAME", "GID", "MEMBERS", "DESCRIPTION"}
}

// Rows implements TableRenderer.
func (gl GroupList) Rows() [][]string {
	rows := make([][]string, 0, len(gl))
	for _, g := range gl {
		members := cmdutil.EmptyOr(strings.Join(g.Members, ", "), "-")
		description := cmdutil.EmptyOr(g.Description, "-")
		gidStr := "-"
		if g.GID != nil {
			gidStr = fmt.Sprintf("%d", *g.GID)
		}
		rows = append(rows, []string{g.Name, gidStr, members, description})
	}
	return rows
}

func runList(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	groups, err := client.ListGroups()
	if err != nil {
		return fmt.Errorf("failed to list groups: %w", err)
	}

	return cmdutil.PrintOutput(os.Stdout, groups, len(groups) == 0, "No groups found.", GroupList(groups))
}
