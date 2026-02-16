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
)

var revokeCmd = &cobra.Command{
	Use:   "revoke <share>",
	Short: "Revoke permission from a share",
	Long: `Revoke permission from a user or group on a share.

Examples:
  # Revoke permission from user
  dfsctl share permission revoke /archive --user alice

  # Revoke permission from group
  dfsctl share permission revoke /archive --group editors`,
	Args: cobra.ExactArgs(1),
	RunE: runRevoke,
}

func init() {
	revokeCmd.Flags().StringVar(&revokeUser, "user", "", "Username to revoke permission from")
	revokeCmd.Flags().StringVar(&revokeGroup, "group", "", "Group name to revoke permission from")
}

func runRevoke(cmd *cobra.Command, args []string) error {
	shareName := args[0]

	if revokeUser == "" && revokeGroup == "" {
		return fmt.Errorf("either --user or --group must be specified")
	}
	if revokeUser != "" && revokeGroup != "" {
		return fmt.Errorf("--user and --group are mutually exclusive")
	}

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	var target string
	if revokeUser != "" {
		if err := client.RemoveUserSharePermission(shareName, revokeUser); err != nil {
			return fmt.Errorf("failed to revoke permission: %w", err)
		}
		target = fmt.Sprintf("user '%s'", revokeUser)
	} else {
		if err := client.RemoveGroupSharePermission(shareName, revokeGroup); err != nil {
			return fmt.Errorf("failed to revoke permission: %w", err)
		}
		target = fmt.Sprintf("group '%s'", revokeGroup)
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
