package share

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var disableCmd = &cobra.Command{
	Use:   "disable <name>",
	Short: "Disable a share (drain clients, block new connections)",
	Long: `Disable a share on the DittoFS server.

Disabling a share drains connected clients synchronously (NFS MOUNT / NFSv4
PUTFH / SMB TREE_CONNECT are refused for disabled shares) and blocks new
connections until the share is re-enabled. This is the safety gate that
must precede a metadata-store restore.

The command blocks until the drain completes (or the server's
lifecycle shutdown timeout fires). Exit code is 0 when the share has been
marked disabled and all in-flight clients have been notified.

Examples:
  # Disable a share before restoring its metadata store
  dfsctl share /archive disable

  # Emit the updated Share record as JSON
  dfsctl share /archive disable -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runDisable,
}

func runDisable(cmd *cobra.Command, args []string) error {
	name := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	share, err := client.DisableShare(name)
	if err != nil {
		return fmt.Errorf("failed to disable share: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, share, fmt.Sprintf("Share %s disabled.", name))
}
