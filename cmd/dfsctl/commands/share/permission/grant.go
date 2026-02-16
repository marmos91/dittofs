package permission

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/spf13/cobra"
)

var (
	grantUser  string
	grantGroup string
	grantLevel string
)

var grantCmd = &cobra.Command{
	Use:   "grant <share>",
	Short: "Grant permission on a share",
	Long: `Grant permission to a user or group on a share.

Permission levels:
  - none: No access
  - read: Read-only access
  - read-write: Read and write access
  - admin: Full administrative access

Examples:
  # Grant read-write to user
  dfsctl share permission grant /archive --user alice --level read-write

  # Grant read to group
  dfsctl share permission grant /archive --group editors --level read`,
	Args: cobra.ExactArgs(1),
	RunE: runGrant,
}

func init() {
	grantCmd.Flags().StringVar(&grantUser, "user", "", "Username to grant permission to")
	grantCmd.Flags().StringVar(&grantGroup, "group", "", "Group name to grant permission to")
	grantCmd.Flags().StringVar(&grantLevel, "level", "", "Permission level (none|read|read-write|admin)")
	_ = grantCmd.MarkFlagRequired("level")
}

func runGrant(cmd *cobra.Command, args []string) error {
	shareName := args[0]

	if grantUser == "" && grantGroup == "" {
		return fmt.Errorf("either --user or --group must be specified")
	}
	if grantUser != "" && grantGroup != "" {
		return fmt.Errorf("--user and --group are mutually exclusive")
	}

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	var target string
	if grantUser != "" {
		if err := client.SetUserSharePermission(shareName, grantUser, grantLevel); err != nil {
			return fmt.Errorf("failed to grant permission: %w", err)
		}
		target = fmt.Sprintf("user '%s'", grantUser)
	} else {
		if err := client.SetGroupSharePermission(shareName, grantGroup, grantLevel); err != nil {
			return fmt.Errorf("failed to grant permission: %w", err)
		}
		target = fmt.Sprintf("group '%s'", grantGroup)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	if format == output.FormatTable {
		printer := output.NewPrinter(os.Stdout, format, !cmdutil.IsColorDisabled())
		printer.Success(fmt.Sprintf("Granted '%s' permission to %s on '%s'", grantLevel, target, shareName))
	}

	return nil
}
