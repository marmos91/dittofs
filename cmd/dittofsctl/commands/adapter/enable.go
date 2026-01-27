package adapter

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var enablePort int

var enableCmd = &cobra.Command{
	Use:   "enable <type>",
	Short: "Enable an adapter",
	Long: `Enable a protocol adapter on the DittoFS server.

If the adapter doesn't exist, it will be created.

Examples:
  # Enable NFS adapter with default port
  dittofsctl adapter enable nfs

  # Enable NFS adapter on specific port
  dittofsctl adapter enable nfs --port 2049

  # Enable SMB adapter
  dittofsctl adapter enable smb --port 445`,
	Args: cobra.ExactArgs(1),
	RunE: runEnable,
}

func init() {
	enableCmd.Flags().IntVar(&enablePort, "port", 0, "Listen port (uses default if not specified)")
}

func runEnable(cmd *cobra.Command, args []string) error {
	adapterType := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	// Try to update first
	enabled := true
	updateReq := &apiclient.UpdateAdapterRequest{
		Enabled: &enabled,
	}
	if enablePort > 0 {
		updateReq.Port = &enablePort
	}

	adapter, err := client.UpdateAdapter(adapterType, updateReq)
	if err != nil {
		// If adapter doesn't exist, create it
		if apiErr, ok := err.(*apiclient.APIError); ok && apiErr.IsNotFound() {
			createReq := &apiclient.CreateAdapterRequest{
				Type:    adapterType,
				Enabled: &enabled,
				Port:    enablePort,
			}
			adapter, err = client.CreateAdapter(createReq)
			if err != nil {
				return fmt.Errorf("failed to create adapter: %w", err)
			}
		} else {
			return fmt.Errorf("failed to enable adapter: %w", err)
		}
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, adapter, fmt.Sprintf("Adapter '%s' enabled on port %d", adapter.Type, adapter.Port))
}
