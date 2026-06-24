package adapter

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var enablePort int

var enableCmd = &cobra.Command{
	Use:   "enable <type>",
	Short: "Enable an adapter",
	Long: `Enable a protocol adapter on the DittoFS server.

If the adapter record does not yet exist it is created automatically. Use --port to override the default listen port (NFS defaults to 12049, SMB to 12445). Changes take effect immediately without a server restart.

Examples:
  # Enable the NFS adapter on the default port (12049)
  dfsctl adapter enable nfs

  # Enable the NFS adapter on a custom port
  dfsctl adapter enable nfs --port 12049

  # Enable the SMB adapter on the default port (12445)
  dfsctl adapter enable smb --port 12445`,
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
				// Handle race: adapter was created between our update and create calls
				if apiErr2, ok := err.(*apiclient.APIError); ok && apiErr2.IsConflict() {
					adapter, err = client.UpdateAdapter(adapterType, updateReq)
				}
				if err != nil {
					return fmt.Errorf("failed to enable adapter: %w", err)
				}
			}
		} else {
			return fmt.Errorf("failed to enable adapter: %w", err)
		}
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, adapter, fmt.Sprintf("Adapter '%s' enabled on port %d", adapter.Type, adapter.Port))
}
