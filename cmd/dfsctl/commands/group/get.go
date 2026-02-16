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
	Long: `Get detailed information about a group.

Examples:
  # Get group details as table
  dfsctl group get admins

  # Get as JSON
  dfsctl group get admins -o json`,
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
