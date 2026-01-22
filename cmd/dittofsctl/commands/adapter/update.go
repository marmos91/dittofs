package adapter

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	updatePort    int
	updateEnabled string
	updateConfig  string
)

var updateCmd = &cobra.Command{
	Use:   "update <type>",
	Short: "Update an adapter",
	Long: `Update an existing protocol adapter on the DittoFS server.

Only specified fields will be updated.

Examples:
  # Update port
  dittofsctl adapter update nfs --port 3049

  # Enable/disable adapter
  dittofsctl adapter update smb --enabled false

  # Update configuration
  dittofsctl adapter update nfs --config '{"read_size":65536}'`,
	Args: cobra.ExactArgs(1),
	RunE: runUpdate,
}

func init() {
	updateCmd.Flags().IntVar(&updatePort, "port", 0, "Listen port")
	updateCmd.Flags().StringVar(&updateEnabled, "enabled", "", "Enable/disable adapter (true|false)")
	updateCmd.Flags().StringVar(&updateConfig, "config", "", "Adapter configuration as JSON")
}

func runUpdate(cmd *cobra.Command, args []string) error {
	adapterType := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	req := &apiclient.UpdateAdapterRequest{}
	hasUpdate := false

	if updatePort > 0 {
		req.Port = &updatePort
		hasUpdate = true
	}

	if updateEnabled != "" {
		enabled := strings.ToLower(updateEnabled) == "true"
		req.Enabled = &enabled
		hasUpdate = true
	}

	if updateConfig != "" {
		var config any
		if err := json.Unmarshal([]byte(updateConfig), &config); err != nil {
			return fmt.Errorf("invalid JSON config: %w", err)
		}
		req.Config = config
		hasUpdate = true
	}

	if !hasUpdate {
		return fmt.Errorf("no update fields specified. Use --port, --enabled, or --config")
	}

	adapter, err := client.UpdateAdapter(adapterType, req)
	if err != nil {
		return fmt.Errorf("failed to update adapter: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, adapter, fmt.Sprintf("Adapter '%s' updated successfully", adapter.Type))
}
