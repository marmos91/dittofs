package share

import (
	"fmt"
	"os"
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

This controls protocol-specific NFS export settings such as the squash mode,
auth flavor, and the netgroup that restricts which clients may access the
export. Netgroup changes take effect immediately; other fields apply on the
next adapter restart.

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
	Args:  cobra.ExactArgs(1),
	RunE:  runNFSConfigShow,
}

var nfsConfigSetCmd = &cobra.Command{
	Use:   "set <name>",
	Short: "Update a share's NFS adapter configuration",
	Args:  cobra.ExactArgs(1),
	RunE:  runNFSConfigSet,
}

func init() {
	nfsConfigSetCmd.Flags().StringVar(&nfsCfgNetgroup, "netgroup", "", "Netgroup name to associate (empty string clears the association)")
	nfsConfigSetCmd.Flags().StringVar(&nfsCfgSquash, "squash", "", "Squash mode (none|root|all)")
	nfsConfigSetCmd.Flags().StringVar(&nfsCfgAllowAuthSys, "allow-auth-sys", "", "Allow AUTH_SYS flavor (true|false)")
	nfsConfigSetCmd.Flags().StringVar(&nfsCfgRequireKerberos, "require-kerberos", "", "Require Kerberos auth (true|false)")

	nfsConfigCmd.AddCommand(nfsConfigShowCmd)
	nfsConfigCmd.AddCommand(nfsConfigSetCmd)
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

	return cmdutil.PrintResource(os.Stdout, cfg, nil)
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
