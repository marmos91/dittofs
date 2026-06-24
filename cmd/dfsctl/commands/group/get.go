package group

import (
	"fmt"
	"os"
	"strings"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var getCmd = &cobra.Command{
	Use:   "get <name>",
	Short: "Get group details",
	Long: `Get detailed information about a specific group on the DittoFS server.
The output includes the group's GID, description, member list, and creation
timestamp. Use -o json or -o yaml for machine-readable output.

Examples:
  # Show group details as a table
  dfsctl group get editors

  # Output as JSON (useful for scripting)
  dfsctl group get editors -o json

  # Output as YAML
  dfsctl group get editors -o yaml`,
	Args: cobra.ExactArgs(1),
	RunE: runGet,
}

// SingleGroupList wraps a single group for table rendering.
type SingleGroupList []apiclient.Group

// Headers implements TableRenderer.
func (gl SingleGroupList) Headers() []string {
	return []string{"FIELD", "VALUE"}
}

// Rows implements TableRenderer.
func (gl SingleGroupList) Rows() [][]string {
	if len(gl) == 0 {
		return nil
	}
	g := gl[0]
	members := "-"
	if len(g.Members) > 0 {
		members = strings.Join(g.Members, ", ")
	}
	gidStr := "-"
	if g.GID != nil {
		gidStr = fmt.Sprintf("%d", *g.GID)
	}
	description := cmdutil.EmptyOr(g.Description, "-")

	return [][]string{
		{"ID", g.ID},
		{"Name", g.Name},
		{"GID", gidStr},
		{"Description", description},
		{"Members", members},
		{"Created", g.CreatedAt.Format("2006-01-02 15:04:05")},
	}
}

func runGet(cmd *cobra.Command, args []string) error {
	name := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	group, err := client.GetGroup(name)
	if err != nil {
		return fmt.Errorf("failed to get group: %w", err)
	}

	return cmdutil.PrintResource(os.Stdout, group, SingleGroupList{*group})
}
