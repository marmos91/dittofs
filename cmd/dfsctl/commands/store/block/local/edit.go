package local

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
	editType   string
	editConfig string
	editPath   string
)

var editCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Edit a local block store",
	Long: `Edit an existing local block store configuration.

When run without flags, opens an interactive editor to modify store properties.
When flags are provided, only the specified fields are updated.

Examples:
  # Edit interactively
  dfsctl store block local edit default-local

  # Update config with JSON
  dfsctl store block local edit default-local --config '{"path":"/new/path"}'

  # Update path for fs store
  dfsctl store block local edit default-local --path /new/path`,
	Args: cobra.ExactArgs(1),
	RunE: runEdit,
}

func init() {
	editCmd.Flags().StringVar(&editType, "type", "", "Store type: fs, memory")
	editCmd.Flags().StringVar(&editConfig, "config", "", "Store configuration as JSON")
	editCmd.Flags().StringVar(&editPath, "path", "", "Block directory path (for fs type)")
}

func runEdit(cmd *cobra.Command, args []string) error {
	name := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	current, err := client.GetBlockStore("local", name)
	if err != nil {
		return fmt.Errorf("failed to get local block store: %w", err)
	}

	hasFlags := cmd.Flags().Changed("type") || cmd.Flags().Changed("config") || cmd.Flags().Changed("path")

	if !hasFlags {
		return runEditInteractive(client, name, current)
	}

	req := &apiclient.UpdateStoreRequest{}
	hasUpdate := false

	if editConfig != "" {
		var config any
		if err := json.Unmarshal([]byte(editConfig), &config); err != nil {
			return fmt.Errorf("invalid JSON config: %w", err)
		}
		req.Config = config
		hasUpdate = true
	} else if editPath != "" {
		currentConfig := cmdutil.ParseConfigMap(current.Config)
		currentConfig["path"] = editPath
		req.Config = currentConfig
		hasUpdate = true
	}

	if editType != "" {
		req.Type = &editType
		hasUpdate = true
	}

	if !hasUpdate {
		return fmt.Errorf("no update fields specified. Use --type, --config, or --path")
	}

	store, err := client.UpdateBlockStore("local", name, req)
	if err != nil {
		return fmt.Errorf("failed to update local block store: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, store, fmt.Sprintf("Local block store '%s' updated successfully", store.Name))
}

func runEditInteractive(client *apiclient.Client, name string, current *apiclient.BlockStore) error {
	fmt.Printf("Editing local block store: %s (type: %s)\n", current.Name, current.Type)
	fmt.Println("Press Ctrl+C to abort.")
	fmt.Println()

	currentConfig := cmdutil.ParseConfigMap(current.Config)

	req := &apiclient.UpdateStoreRequest{}
	hasUpdate := false

	switch current.Type {
	case "fs":
		path := cmdutil.GetConfigString(currentConfig, "path", "")

		newPath, err := prompt.Input("Block directory path", path)
		if err != nil {
			return cmdutil.HandleAbort(err)
		}

		if newPath != path {
			newConfig := map[string]any{
				"path": newPath,
			}
			req.Config = newConfig
			hasUpdate = true
		}

	case "memory":
		fmt.Println("Memory stores have no configurable settings.")
		return nil

	default:
		return fmt.Errorf("unknown store type: %s", current.Type)
	}

	if !hasUpdate {
		fmt.Println("No changes made.")
		return nil
	}

	store, err := client.UpdateBlockStore("local", name, req)
	if err != nil {
		return fmt.Errorf("failed to update local block store: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, store, fmt.Sprintf("Local block store '%s' updated successfully", store.Name))
}
