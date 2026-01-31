package adapter

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var disableCmd = &cobra.Command{
	Use:   "disable <type>",
	Short: "Disable an adapter",
	Long: `Disable a protocol adapter on the DittoFS server.

Examples:
  # Disable NFS adapter
  dittofsctl adapter disable nfs

  # Disable SMB adapter
  dittofsctl adapter disable smb`,
	Args: cobra.ExactArgs(1),
	RunE: runDisable,
}

func runDisable(cmd *cobra.Command, args []string) error {
	adapterType := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	enabled := false
	req := &apiclient.UpdateAdapterRequest{
		Enabled: &enabled,
	}

	adapter, err := client.UpdateAdapter(adapterType, req)
	if err != nil {
		return fmt.Errorf("failed to disable adapter: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, adapter, fmt.Sprintf("Adapter '%s' disabled", adapter.Type))
}
