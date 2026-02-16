package netgroup

import (
	"fmt"
	"os"
	"strings"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/spf13/cobra"
)

var (
	addMemberType  string
	addMemberValue string
)

var addMemberCmd = &cobra.Command{
	Use:   "add-member <name>",
	Short: "Add a member to a netgroup",
	Long: `Add an IP address, CIDR range, or hostname to a netgroup.

Examples:
  # Add a single IP
  dfsctl netgroup add-member office-network --type ip --value 192.168.1.100

  # Add a CIDR range
  dfsctl netgroup add-member office-network --type cidr --value 10.0.0.0/8

  # Add a hostname
  dfsctl netgroup add-member office-network --type hostname --value server1.example.com`,
	Args: cobra.ExactArgs(1),
	RunE: runAddMember,
}

func init() {
	addMemberCmd.Flags().StringVar(&addMemberType, "type", "", "Member type: ip, cidr, or hostname (required)")
	addMemberCmd.Flags().StringVar(&addMemberValue, "value", "", "Member value (required)")
	_ = addMemberCmd.MarkFlagRequired("type")
	_ = addMemberCmd.MarkFlagRequired("value")
}

func runAddMember(cmd *cobra.Command, args []string) error {
	netgroupName := args[0]

	// Validate member type
	memberType := strings.ToLower(addMemberType)
	if !models.ValidateMemberType(memberType) {
		return fmt.Errorf("invalid member type: %q (valid: %v)", addMemberType, models.ValidMemberTypes)
	}

	// Client-side validation (reuse models validation for consistency)
	if err := models.ValidateMemberValue(memberType, addMemberValue); err != nil {
		return err
	}

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	member, err := client.AddNetgroupMember(netgroupName, memberType, addMemberValue)
	if err != nil {
		return fmt.Errorf("failed to add member to netgroup: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, member,
		fmt.Sprintf("Member %s '%s' added to netgroup '%s' (ID: %s)", memberType, addMemberValue, netgroupName, member.ID))
}
