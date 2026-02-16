package adapter

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	editPort    int
	editEnabled string
	editConfig  string
)

var editCmd = &cobra.Command{
	Use:   "edit <type>",
	Short: "Edit an adapter",
	Long: `Edit an existing protocol adapter on the DittoFS server.

When run without flags, opens an interactive editor to modify adapter properties.
When flags are provided, only the specified fields are updated.

Examples:
  # Edit adapter interactively
  dfsctl adapter edit nfs

  # Update port directly
  dfsctl adapter edit nfs --port 3049

  # Enable/disable adapter
  dfsctl adapter edit smb --enabled false

  # Update configuration
  dfsctl adapter edit nfs --config '{"read_size":65536}'`,
	Args: cobra.ExactArgs(1),
	RunE: runEdit,
}

func init() {
	editCmd.Flags().IntVar(&editPort, "port", 0, "Listen port")
	editCmd.Flags().StringVar(&editEnabled, "enabled", "", "Enable/disable adapter (true|false)")
	editCmd.Flags().StringVar(&editConfig, "config", "", "Adapter configuration as JSON")
}

func runEdit(cmd *cobra.Command, args []string) error {
	adapterType := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	// Check if any flags were provided
	hasFlags := cmd.Flags().Changed("port") || cmd.Flags().Changed("enabled") || cmd.Flags().Changed("config")

	// If no flags provided, run interactive mode
	if !hasFlags {
		return runEditInteractive(client, adapterType)
	}

	req := &apiclient.UpdateAdapterRequest{}
	hasUpdate := false

	if editPort > 0 {
		req.Port = &editPort
		hasUpdate = true
	}

	if editEnabled != "" {
		enabled := strings.ToLower(editEnabled) == "true"
		req.Enabled = &enabled
		hasUpdate = true
	}

	if editConfig != "" {
		var config any
		if err := json.Unmarshal([]byte(editConfig), &config); err != nil {
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

func runEditInteractive(client *apiclient.Client, adapterType string) error {
	// Fetch current adapter
	adapters, err := client.ListAdapters()
	if err != nil {
		return fmt.Errorf("failed to list adapters: %w", err)
	}

	var current *apiclient.Adapter
	for i := range adapters {
		if adapters[i].Type == adapterType {
			current = &adapters[i]
			break
		}
	}

	if current == nil {
		return fmt.Errorf("adapter '%s' not found", adapterType)
	}

	fmt.Printf("Editing adapter: %s\n", current.Type)
	fmt.Println("Press Enter to keep current value, or enter a new value.")
	fmt.Println("Press Ctrl+C to abort.")
	fmt.Println()

	req := &apiclient.UpdateAdapterRequest{}
	hasUpdate := false

	// Port
	currentPortStr := fmt.Sprintf("%d", current.Port)
	newPortStr, err := prompt.Input("Port", currentPortStr)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	if newPortStr != currentPortStr {
		var newPort int
		if _, err := fmt.Sscanf(newPortStr, "%d", &newPort); err == nil {
			req.Port = &newPort
			hasUpdate = true
		}
	}

	// Enabled
	enabledOptions := []prompt.SelectOption{
		{Label: "enabled", Value: "true", Description: "Adapter is running"},
		{Label: "disabled", Value: "false", Description: "Adapter is stopped"},
	}
	currentStatus := "enabled"
	if !current.Enabled {
		currentStatus = "disabled"
	}
	fmt.Printf("Currently: %s\n", currentStatus)
	newEnabledStr, err := prompt.Select("Status", enabledOptions)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	newEnabled := newEnabledStr == "true"
	if newEnabled != current.Enabled {
		req.Enabled = &newEnabled
		hasUpdate = true
	}

	if !hasUpdate {
		fmt.Println("No changes made.")
		return nil
	}

	adapter, err := client.UpdateAdapter(adapterType, req)
	if err != nil {
		return fmt.Errorf("failed to update adapter: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, adapter, fmt.Sprintf("Adapter '%s' updated successfully", adapter.Type))
}
