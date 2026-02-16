package permission

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list <share>",
	Short: "List permissions on a share",
	Long: `List all permissions configured on a share.

Examples:
  # List permissions as table
  dfsctl share permission list /archive

  # List as JSON
  dfsctl share permission list /archive -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runList,
}

// PermissionList is a list of permissions for table rendering.
type PermissionList []apiclient.SharePermission

// Headers implements TableRenderer.
func (pl PermissionList) Headers() []string {
	return []string{"TYPE", "NAME", "LEVEL"}
}

// Rows implements TableRenderer.
func (pl PermissionList) Rows() [][]string {
	rows := make([][]string, 0, len(pl))
	for _, p := range pl {
		rows = append(rows, []string{p.Type, p.Name, p.Level})
	}
	return rows
}

func runList(cmd *cobra.Command, args []string) error {
	shareName := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	perms, err := client.ListSharePermissions(shareName)
	if err != nil {
		return fmt.Errorf("failed to list permissions: %w", err)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, perms)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, perms)
	default:
		if len(perms) == 0 {
			fmt.Printf("No permissions configured on '%s'.\n", shareName)
			return nil
		}
		fmt.Printf("Permissions on '%s':\n", shareName)
		return output.PrintTable(os.Stdout, PermissionList(perms))
	}
}
