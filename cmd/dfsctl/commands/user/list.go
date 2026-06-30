package user

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
	Short: "List all users",
	Long: `List all local users registered on the DittoFS server. The table view
shows each user's username, UID, role, email, group memberships, and whether
the account is enabled. Use -o json or -o yaml to get machine-readable output
suitable for piping into other tools.

Examples:
  # List all users as a table
  dfsctl user list

  # Output the full user list as JSON
  dfsctl user list -o json

  # Output as YAML
  dfsctl user list -o yaml`,
	RunE: runList,
}

// UserList is a list of users for table rendering.
type UserList []apiclient.User

// Headers implements TableRenderer.
func (ul UserList) Headers() []string {
	return []string{"USERNAME", "ID", "UID", "ROLE", "EMAIL", "GROUPS", "ENABLED"}
}

// Rows implements TableRenderer.
func (ul UserList) Rows() [][]string {
	rows := make([][]string, 0, len(ul))
	for _, u := range ul {
		groups := cmdutil.EmptyOr(strings.Join(u.Groups, ", "), "-")
		email := cmdutil.EmptyOr(u.Email, "-")
		uidStr := "-"
		if u.UID != nil {
			uidStr = fmt.Sprintf("%d", *u.UID)
		}
		rows = append(rows, []string{u.Username, u.ID, uidStr, u.Role, email, groups, cmdutil.BoolToYesNo(u.Enabled)})
	}
	return rows
}

func runList(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	users, err := client.ListUsers()
	if err != nil {
		return fmt.Errorf("failed to list users: %w", err)
	}

	return cmdutil.PrintOutput(os.Stdout, users, len(users) == 0, "No users found.", UserList(users))
}
