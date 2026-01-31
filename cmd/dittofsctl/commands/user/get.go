package user

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var getCmd = &cobra.Command{
	Use:   "get <username>",
	Short: "Get user details",
	Long: `Get detailed information about a user.

Examples:
  # Get user details as table
  dittofsctl user get alice

  # Get as JSON
  dittofsctl user get alice -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runGet,
}

// SingleUserList wraps a single user for table rendering.
type SingleUserList []apiclient.User

// Headers implements TableRenderer.
func (ul SingleUserList) Headers() []string {
	return []string{"FIELD", "VALUE"}
}

// Rows implements TableRenderer.
func (ul SingleUserList) Rows() [][]string {
	if len(ul) == 0 {
		return nil
	}
	u := ul[0]
	groups := "-"
	if len(u.Groups) > 0 {
		groups = fmt.Sprintf("%v", u.Groups)
	}

	return [][]string{
		{"ID", u.ID},
		{"Username", u.Username},
		{"Display Name", u.DisplayName},
		{"Email", u.Email},
		{"Role", u.Role},
		{"Groups", groups},
		{"Enabled", cmdutil.BoolToYesNo(u.Enabled)},
		{"Must Change Password", cmdutil.BoolToYesNo(u.MustChangePassword)},
	}
}

func runGet(cmd *cobra.Command, args []string) error {
	username := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	user, err := client.GetUser(username)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}

	return cmdutil.PrintResource(os.Stdout, user, SingleUserList{*user})
}
