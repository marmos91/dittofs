package netgroup

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/spf13/cobra"
)

var removeMemberID string

var removeMemberCmd = &cobra.Command{
	Use:   "remove-member <name>",
	Short: "Remove a member from a netgroup",
	Long: `Remove a member from a netgroup by its member ID. Members are identified
by UUID, not by their IP/CIDR/hostname value — run "dfsctl netgroup show
<name>" to find the ID of the entry you want to remove. The removal takes
effect immediately for subsequent share access checks.

Examples:
  # Find the member ID first
  dfsctl netgroup show office-network

  # Remove a member by its UUID
  dfsctl netgroup remove-member office-network --member-id 550e8400-e29b-41d4-a716-446655440000

  # Remove a member non-interactively in a script
  dfsctl netgroup remove-member datacenter-hosts --member-id a1b2c3d4-e5f6-7890-abcd-ef1234567890`,
	Args: cobra.ExactArgs(1),
	RunE: runRemoveMember,
}

func init() {
	removeMemberCmd.Flags().StringVar(&removeMemberID, "member-id", "", "Member ID to remove (required)")
	_ = removeMemberCmd.MarkFlagRequired("member-id")
}

func runRemoveMember(cmd *cobra.Command, args []string) error {
	netgroupName := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	if err := client.RemoveNetgroupMember(netgroupName, removeMemberID); err != nil {
		return fmt.Errorf("failed to remove member from netgroup: %w", err)
	}

	format, fmtErr := cmdutil.GetOutputFormatParsed()
	if fmtErr != nil {
		return fmtErr
	}

	if format == output.FormatTable {
		printer := output.NewPrinter(os.Stdout, format, !cmdutil.IsColorDisabled())
		printer.Success(fmt.Sprintf("Member '%s' removed from netgroup '%s'", removeMemberID, netgroupName))
	}

	return nil
}
