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
	Long: `Add a network endpoint to a netgroup. The endpoint can be a single IP
address, a CIDR range, or a hostname. Valid types are "ip", "cidr", and
"hostname". Each entry receives a unique ID that you use when removing it
with "dfsctl netgroup remove-member".

Examples:
  # Add a single IP address to the netgroup
  dfsctl netgroup add-member office-network --type ip --value 192.168.1.100

  # Add an entire subnet via CIDR
  dfsctl netgroup add-member office-network --type cidr --value 10.0.0.0/8

  # Add a specific hostname
  dfsctl netgroup add-member office-network --type hostname --value server1.example.com

  # Add a /24 subnet for a datacenter hosts group
  dfsctl netgroup add-member datacenter-hosts --type cidr --value 172.16.0.0/24`,
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
