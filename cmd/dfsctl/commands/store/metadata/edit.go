package metadata

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	editConfig string
	editDBPath string
)

var editCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Edit a metadata store",
	Long: `Edit an existing metadata store configuration.

When run without flags, opens an interactive editor to modify store properties.
When flags are provided, only the specified fields are updated.

Examples:
  # Edit interactively (default)
  dfsctl store metadata edit default

  # Update config with JSON
  dfsctl store metadata edit default --config '{"path":"/new/path"}'

  # Update BadgerDB path
  dfsctl store metadata edit default --db-path /new/path`,
	Args: cobra.ExactArgs(1),
	RunE: runEdit,
}

func init() {
	editCmd.Flags().StringVar(&editConfig, "config", "", "Store configuration as JSON")
	editCmd.Flags().StringVar(&editDBPath, "db-path", "", "BadgerDB directory path")
}

func runEdit(cmd *cobra.Command, args []string) error {
	name := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	// Get current store to show existing values
	current, err := client.GetMetadataStore(name)
	if err != nil {
		return fmt.Errorf("failed to get metadata store: %w", err)
	}

	// Check if any flags were provided
	hasFlags := cmd.Flags().Changed("config") || cmd.Flags().Changed("db-path")

	// If no flags provided, run interactive mode
	if !hasFlags {
		return runEditInteractive(client, name, current)
	}

	req := &apiclient.UpdateStoreRequest{}
	hasUpdate := false

	// Build config from flags
	if editConfig != "" {
		var config any
		if err := json.Unmarshal([]byte(editConfig), &config); err != nil {
			return fmt.Errorf("invalid JSON config: %w", err)
		}
		req.Config = config
		hasUpdate = true
	} else if editDBPath != "" {
		req.Config = map[string]any{"path": editDBPath}
		hasUpdate = true
	}

	if !hasUpdate {
		return fmt.Errorf("no update fields specified. Use --config or --db-path")
	}

	store, err := client.UpdateMetadataStore(name, req)
	if err != nil {
		return fmt.Errorf("failed to update metadata store: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, store, fmt.Sprintf("Metadata store '%s' updated successfully", store.Name))
}

func runEditInteractive(client *apiclient.Client, name string, current *apiclient.MetadataStore) error {
	fmt.Printf("Editing metadata store: %s (type: %s)\n", current.Name, current.Type)
	fmt.Println("Press Ctrl+C to abort.")
	fmt.Println()

	// Parse current config
	var currentConfig map[string]any
	if len(current.Config) > 0 {
		_ = json.Unmarshal(current.Config, &currentConfig)
	}

	req := &apiclient.UpdateStoreRequest{}
	hasUpdate := false

	if current.Type != "badger" {
		return fmt.Errorf("unknown store type: %s", current.Type)
	}
	if inMemory, _ := currentConfig["in_memory"].(bool); inMemory {
		fmt.Println("In-memory stores have no configurable settings.")
		return nil
	}

	currentPath := cmdutil.GetConfigString(currentConfig, "path", "")
	newPath, err := prompt.Input("Database path", currentPath)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}
	if newPath != currentPath {
		req.Config = map[string]any{"path": newPath}
		hasUpdate = true
	}

	if !hasUpdate {
		fmt.Println("No changes made.")
		return nil
	}

	store, err := client.UpdateMetadataStore(name, req)
	if err != nil {
		return fmt.Errorf("failed to update metadata store: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, store, fmt.Sprintf("Metadata store '%s' updated successfully", store.Name))
}
