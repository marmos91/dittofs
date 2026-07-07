package permission

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/spf13/cobra"
)

var (
	revokeUser  string
	revokeGroup string
	revokeSID   string
)

var revokeCmd = &cobra.Command{
	Use:   "revoke <share>",
	Short: "Revoke permission from a share",
	Long: `Remove a per-principal permission entry from a share.

After revoking, the user or group falls back to the share's default permission
level (see 'dfsctl share show'). To explicitly block a principal rather than
fall back to the default, use 'dfsctl share permission grant ... --level none'
instead. Specify exactly one of --user, --group, or --sid.

--user / --group accept a local name, an AD name, or a raw SID (matching the
grant command); --sid revokes a raw SID grant directly.

Examples:
  # Revoke a user's explicit permission (they fall back to the share default)
  dfsctl share permission revoke /archive --user alice

  # Revoke a group's explicit permission
  dfsctl share permission revoke /archive --group editors

  # Revoke a direct AD/SID grant
  dfsctl share permission revoke /archive --sid S-1-5-21-1111-2222-3333-1104`,
	Args: cobra.ExactArgs(1),
	RunE: runRevoke,
}

func init() {
	revokeCmd.Flags().StringVar(&revokeUser, "user", "", "User to revoke permission from (local name, AD name, or SID)")
	revokeCmd.Flags().StringVar(&revokeGroup, "group", "", "Group to revoke permission from (local name, AD name, or SID)")
	revokeCmd.Flags().StringVar(&revokeSID, "sid", "", "Raw Windows SID to revoke permission from")
}

func runRevoke(cmd *cobra.Command, args []string) error {
	shareName := args[0]

	kind, value, _, target, err := selectPrincipal(revokeUser, revokeGroup, revokeSID)
	if err != nil {
		return err
	}

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	switch kind {
	case principalSID:
		err = client.RemoveSIDSharePermission(shareName, value)
	case principalUserName:
		err = client.RemoveUserSharePermission(shareName, value)
	case principalGroupName:
		err = client.RemoveGroupSharePermission(shareName, value)
	}
	if err != nil {
		return fmt.Errorf("failed to revoke permission: %w", err)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	if format == output.FormatTable {
		printer := output.NewPrinter(os.Stdout, format, !cmdutil.IsColorDisabled())
		printer.Success(fmt.Sprintf("Revoked permission from %s on '%s'", target, shareName))
	}

	return nil
}
