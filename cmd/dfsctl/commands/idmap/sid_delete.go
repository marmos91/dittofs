package idmap

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var sidDeleteForce bool

var sidDeleteCmd = &cobra.Command{
	Use:   "delete <sid>",
	Short: "Delete a foreign-SID UID/GID allocation",
	Long: `Delete a durable foreign-SID to Unix UID/GID allocation. This is an
administrative escape hatch: once removed, the SID will be re-allocated to a
potentially different UID/GID on its next resolution, which can re-attribute
files owned by the old Unix ID to a different numeric owner. Use only when
correcting a misallocated SID, and be aware that in-flight NFS/SMB sessions
may cache the old mapping until they reconnect. You will be prompted for
confirmation unless --force is specified.

Examples:
  # Delete a SID allocation (prompts for confirmation)
  dfsctl idmap sid delete S-1-5-21-111-222-333-1107

  # Delete without confirmation (for automated cleanup scripts)
  dfsctl idmap sid delete S-1-5-21-111-222-333-1107 --force`,
	Args: cobra.ExactArgs(1),
	RunE: runSidDelete,
}

func init() {
	sidDeleteCmd.Flags().BoolVarP(&sidDeleteForce, "force", "f", false, "Skip confirmation prompt")
}

func runSidDelete(cmd *cobra.Command, args []string) error {
	sid := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	return cmdutil.RunDeleteWithConfirmation("SID mapping", sid, sidDeleteForce, func() error {
		if err := client.DeleteSIDMapping(sid); err != nil {
			return fmt.Errorf("failed to delete SID mapping: %w", err)
		}
		return nil
	})
}
