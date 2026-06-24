package group

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/spf13/cobra"
)

var removeUserCmd = &cobra.Command{
	Use:   "remove-user <group> <username>",
	Short: "Remove a user from a group",
	Long: `Remove a user from a group on the DittoFS server. The user loses any
permissions that were derived solely from that group membership. Both the
group name and the username are required positional arguments.

Examples:
  # Remove alice from the editors group
  dfsctl group remove-user editors alice

  # Remove a user from the admins group
  dfsctl group remove-user admins bob

  # Verify membership after removal
  dfsctl group get editors`,
	Args: cobra.ExactArgs(2),
	RunE: runRemoveUser,
}

func runRemoveUser(cmd *cobra.Command, args []string) error {
	groupName := args[0]
	username := args[1]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	if err := client.RemoveGroupMember(groupName, username); err != nil {
		return fmt.Errorf("failed to remove user from group: %w", err)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	if format == output.FormatTable {
		printer := output.NewPrinter(os.Stdout, format, !cmdutil.IsColorDisabled())
		printer.Success(fmt.Sprintf("User '%s' removed from group '%s'", username, groupName))
	}

	return nil
}
