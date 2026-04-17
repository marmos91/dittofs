package share

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var enableCmd = &cobra.Command{
	Use:   "enable <name>",
	Short: "Enable a share (accept new connections)",
	Long: `Enable a share on the DittoFS server.

Re-enabling a share allows new client connections and lifts the drain state
set by 'share disable'. Re-enabling is a deliberate operator act; no
mid-restore safety check is performed — the operator owns the timing.

Examples:
  # Enable a share after a completed metadata-store restore
  dfsctl share /archive enable

  # Emit the updated Share record as JSON
  dfsctl share /archive enable -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runEnable,
}

func runEnable(cmd *cobra.Command, args []string) error {
	name := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	share, err := client.EnableShare(name)
	if err != nil {
		return fmt.Errorf("failed to enable share: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, share, fmt.Sprintf("Share %s enabled.", name))
}
