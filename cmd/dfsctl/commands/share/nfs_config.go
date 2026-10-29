package share

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	nfsCfgNetgroup        string
	nfsCfgSquash          string
	nfsCfgAllowAuthSys    string
	nfsCfgRequireKerberos string
)

// nfsConfigCmd is the parent for per-share NFS adapter config.
var nfsConfigCmd = &cobra.Command{
	Use:   "nfs-config",
	Short: "Manage per-share NFS adapter configuration",
	Long: `View and update the NFS adapter configuration for a share.

This controls the share's export settings: the squash mode, the
authentication flags and the netgroup that restricts which clients may reach
the share. The authentication flags and the netgroup are the share's access
policy, which applies over NFS and SMB alike; the squash mode maps the
identities of SMB sessions too, guest sessions excepted. Changes take effect
immediately.

Examples:
  # Show a share's NFS config
  dfsctl share nfs-config show /export

  # Associate a netgroup with the share's NFS export
  dfsctl share nfs-config set /export --netgroup office-network

  # Remove the netgroup association (allow all clients)
  dfsctl share nfs-config set /export --netgroup ""`,
}

var nfsConfigShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show a share's NFS adapter configuration",
	Long: `Show the NFS adapter configuration for a share.

Displays the netgroup association, squash mode, and authentication settings
(AUTH_SYS and Kerberos). Use this command to inspect the current NFS export
settings before making changes with 'nfs-config set'.

Examples:
  # Show NFS config for a share
  dfsctl share nfs-config show /export

  # Emit as JSON
  dfsctl share nfs-config show /export -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runNFSConfigShow,
}

var nfsConfigSetCmd = &cobra.Command{
	Use:   "set <name>",
	Short: "Update a share's NFS adapter configuration",
	Long: `Update the NFS adapter configuration for a share.

Only the flags you supply are changed; omitted flags leave the existing values
intact. Changes take effect immediately, without an adapter restart.

The netgroup allowlist and the authentication flags are the share's access
policy: they are enforced on NFSv3 mounts, on NFSv4 operations and on SMB
requests to the same share. The squash mode is not NFS-only either: it maps the
identities of SMB sessions on the share as well, guest sessions excepted.

Examples:
  # Restrict access to a specific netgroup
  dfsctl share nfs-config set /export --netgroup office-network

  # Remove the netgroup restriction (allow all clients)
  dfsctl share nfs-config set /export --netgroup ""

  # Map root UID to guest on this export
  dfsctl share nfs-config set /export --squash root_to_guest

  # Require Kerberos authentication and disallow AUTH_SYS
  dfsctl share nfs-config set /export --require-kerberos true --allow-auth-sys false`,
	Args: cobra.ExactArgs(1),
	RunE: runNFSConfigSet,
}

func init() {
	nfsConfigSetCmd.Flags().StringVar(&nfsCfgNetgroup, "netgroup", "", "Netgroup name to associate (empty string clears the association)")
	nfsConfigSetCmd.Flags().StringVar(&nfsCfgSquash, "squash", "", "Squash mode (none|root_to_admin|root_to_guest|all_to_admin|all_to_guest)")
	nfsConfigSetCmd.Flags().StringVar(&nfsCfgAllowAuthSys, "allow-auth-sys", "", "Allow AUTH_SYS flavor (true|false)")
	nfsConfigSetCmd.Flags().StringVar(&nfsCfgRequireKerberos, "require-kerberos", "", "Require Kerberos auth (true|false)")

	nfsConfigCmd.AddCommand(nfsConfigShowCmd)
	nfsConfigCmd.AddCommand(nfsConfigSetCmd)
}

// NFSConfigDetail renders a share's NFS adapter config as a field/value table.
type NFSConfigDetail struct {
	cfg *apiclient.ShareNFSConfig
}

// Headers implements TableRenderer.
func (d NFSConfigDetail) Headers() []string {
	return []string{"FIELD", "VALUE"}
}

// Rows implements TableRenderer.
func (d NFSConfigDetail) Rows() [][]string {
	c := d.cfg
	netgroup := c.Netgroup
	if netgroup == "" {
		netgroup = "(none - all clients allowed)"
	}
	rows := [][]string{
		{"Netgroup", netgroup},
		{"Squash", c.Squash},
	}
	// The anonymous IDs only apply to a squash mode that maps to them, and are
	// server defaults when unset — show them only when the export overrides them.
	if c.AnonymousUID != nil {
		rows = append(rows, []string{"Anonymous UID", fmt.Sprintf("%d", *c.AnonymousUID)})
	}
	if c.AnonymousGID != nil {
		rows = append(rows, []string{"Anonymous GID", fmt.Sprintf("%d", *c.AnonymousGID)})
	}
	return append(rows,
		[]string{"Allow AUTH_SYS", strconv.FormatBool(c.AllowAuthSys)},
		[]string{"Require Kerberos", strconv.FormatBool(c.RequireKerberos)},
		[]string{"Min Kerberos level", c.MinKerberosLevel},
		[]string{"READDIRPLUS", strconv.FormatBool(!c.DisableReaddirplus)},
	)
}

func runNFSConfigShow(_ *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	cfg, err := client.GetShareNFSConfig(args[0])
	if err != nil {
		return fmt.Errorf("failed to get NFS config: %w", err)
	}

	return cmdutil.PrintResource(os.Stdout, cfg, NFSConfigDetail{cfg: cfg})
}

func runNFSConfigSet(cmd *cobra.Command, args []string) error {
	name := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	req := &apiclient.PatchShareNFSConfigRequest{}
	hasUpdate := false

	// --netgroup is a pointer-to-string so an explicit empty value clears the
	// association. Only act when the flag was actually provided.
	if cmd.Flags().Changed("netgroup") {
		ng := nfsCfgNetgroup
		req.Netgroup = &ng
		hasUpdate = true
	}

	if nfsCfgSquash != "" {
		req.Squash = &nfsCfgSquash
		hasUpdate = true
	}

	if nfsCfgAllowAuthSys != "" {
		v, err := parseBoolFlag("allow-auth-sys", nfsCfgAllowAuthSys)
		if err != nil {
			return err
		}
		req.AllowAuthSys = &v
		hasUpdate = true
	}

	if nfsCfgRequireKerberos != "" {
		v, err := parseBoolFlag("require-kerberos", nfsCfgRequireKerberos)
		if err != nil {
			return err
		}
		req.RequireKerberos = &v
		hasUpdate = true
	}

	if !hasUpdate {
		return fmt.Errorf("no fields specified. Use --netgroup, --squash, --allow-auth-sys, or --require-kerberos")
	}

	cfg, err := client.PatchShareNFSConfig(name, req)
	if err != nil {
		return fmt.Errorf("failed to update NFS config: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, cfg, fmt.Sprintf("NFS config for share '%s' updated successfully", name))
}

func parseBoolFlag(flag, value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("--%s: invalid value %q, must be true or false", flag, value)
	}
}
