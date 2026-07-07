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
	grantSID   string
	grantLevel string
)

var grantCmd = &cobra.Command{
	Use:   "grant <share>",
	Short: "Grant permission on a share",
	Long: `Grant a permission level to a user or group on a share.

Specify exactly one of --user, --group, or --sid together with --level.
Re-running the command on a principal that already has a permission replaces the
existing level. Permission levels in order of increasing access:
  - none:       No access (explicitly blocks the principal)
  - read:       Read-only access
  - read-write: Read and write access
  - admin:      Full administrative access including ACL management

Active Directory principals can be granted directly, with no local DittoFS
account (issue #1528). --user / --group accept a local name, an AD name
(user@REALM or DOMAIN\group, resolved to a SID via the configured LDAP
directory), or a raw Windows SID. A bare name resolves to a local user/group if
one exists, otherwise to the directory. --sid grants to a raw SID explicitly.

Examples:
  # Grant read-write access to a local user
  dfsctl share permission grant /archive --user alice --level read-write

  # Grant read-only access to a local group
  dfsctl share permission grant /archive --group editors --level read

  # Grant directly to an AD group (resolved to its SID via LDAP) — no local group
  dfsctl share permission grant /archive --group 'CUBBIT\Cubbit' --level read-write

  # Grant directly to an AD user by Kerberos principal
  dfsctl share permission grant /archive --user alice@cubbit.local --level read

  # Grant to a raw Windows SID (no directory lookup)
  dfsctl share permission grant /archive --sid S-1-5-21-1111-2222-3333-1104 --level read`,
	Args: cobra.ExactArgs(1),
	RunE: runGrant,
}

func init() {
	grantCmd.Flags().StringVar(&grantUser, "user", "", "User to grant permission to (local name, AD name, or SID)")
	grantCmd.Flags().StringVar(&grantGroup, "group", "", "Group to grant permission to (local name, AD name, or SID)")
	grantCmd.Flags().StringVar(&grantSID, "sid", "", "Raw Windows SID to grant permission to (e.g. S-1-5-21-...)")
	grantCmd.Flags().StringVar(&grantLevel, "level", "", "Permission level (none|read|read-write|admin)")
	_ = grantCmd.MarkFlagRequired("level")
}

func runGrant(cmd *cobra.Command, args []string) error {
	shareName := args[0]

	// A raw SID short-circuits to the SID endpoint; a name goes to the local
	// user/group endpoint, which falls back to the directory when no local
	// object exists (#1528).
	kind, value, isGroup, target, err := selectPrincipal(grantUser, grantGroup, grantSID)
	if err != nil {
		return err
	}

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	switch kind {
	case principalSID:
		err = client.SetSIDSharePermission(shareName, value, grantLevel, isGroup, "")
	case principalUserName:
		err = client.SetUserSharePermission(shareName, value, grantLevel)
	case principalGroupName:
		err = client.SetGroupSharePermission(shareName, value, grantLevel)
	}
	if err != nil {
		return fmt.Errorf("failed to grant permission: %w", err)
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
