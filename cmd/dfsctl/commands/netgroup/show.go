package netgroup

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var showCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show netgroup details",
	Long: `Show detailed information about a netgroup including all members.

Examples:
  # Show netgroup details
  dfsctl netgroup show office-network

  # Show as JSON
  dfsctl netgroup show office-network -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runShow,
}

// NetgroupDetail wraps a netgroup for detailed table rendering.
type NetgroupDetail struct {
	netgroup *apiclient.Netgroup
}

// Headers implements TableRenderer.
func (nd NetgroupDetail) Headers() []string {
	return []string{"FIELD", "VALUE"}
}

// Rows implements TableRenderer.
func (nd NetgroupDetail) Rows() [][]string {
	n := nd.netgroup
	rows := [][]string{
		{"ID", n.ID},
		{"Name", n.Name},
		{"Members", fmt.Sprintf("%d", len(n.Members))},
		{"Created", n.CreatedAt.Format("2006-01-02 15:04:05")},
		{"Updated", n.UpdatedAt.Format("2006-01-02 15:04:05")},
	}
	return rows
}

// MemberList renders netgroup members as a table.
type MemberList []apiclient.NetgroupMember

// Headers implements TableRenderer.
func (ml MemberList) Headers() []string {
	return []string{"ID", "TYPE", "VALUE"}
}

// Rows implements TableRenderer.
func (ml MemberList) Rows() [][]string {
	rows := make([][]string, 0, len(ml))
	for _, m := range ml {
		rows = append(rows, []string{m.ID, m.Type, m.Value})
	}
	return rows
}

func runShow(cmd *cobra.Command, args []string) error {
	name := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	netgroup, err := client.GetNetgroup(name)
	if err != nil {
		return fmt.Errorf("failed to get netgroup: %w", err)
	}

	format, fmtErr := cmdutil.GetOutputFormatParsed()
	if fmtErr != nil {
		return fmtErr
	}

	// For JSON/YAML, output the whole netgroup
	if format != output.FormatTable {
		return cmdutil.PrintResource(os.Stdout, netgroup, nil)
	}

	// For table format, show info and members separately
	if err := output.PrintTable(os.Stdout, NetgroupDetail{netgroup: netgroup}); err != nil {
		return err
	}

	if len(netgroup.Members) > 0 {
		fmt.Println()
		fmt.Println("Members:")
		return output.PrintTable(os.Stdout, MemberList(netgroup.Members))
	}

	fmt.Println()
	fmt.Println("No members.")
	return nil
}
