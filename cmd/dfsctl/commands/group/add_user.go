package group

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/spf13/cobra"
)

var addUserCmd = &cobra.Command{
	Use:   "add-user <group> <username>",
	Short: "Add a user to a group",
	Long: `Add a user to a group on the DittoFS server. The user will immediately
gain any permissions associated with that group on shares and other resources.
Both the group name and the username are required positional arguments.

Examples:
  # Add alice to the editors group
  dfsctl group add-user editors alice

  # Add a user to the admins group
  dfsctl group add-user admins bob

  # Verify membership after adding
  dfsctl group get editors`,
	Args: cobra.ExactArgs(2),
	RunE: runAddUser,
}

func runAddUser(cmd *cobra.Command, args []string) error {
	groupName := args[0]
	username := args[1]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	if err := client.AddGroupMember(groupName, username); err != nil {
		return fmt.Errorf("failed to add user to group: %w", err)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	if format == output.FormatTable {
		printer := output.NewPrinter(os.Stdout, format, !cmdutil.IsColorDisabled())
		printer.Success(fmt.Sprintf("User '%s' added to group '%s'", username, groupName))
	}

	return nil
}
