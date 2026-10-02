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
	addName     string
	addType     string
	addConfig   string
	addDBPath   string
	addInMemory bool
)

var addCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a metadata store",
	Long: `Add a new metadata store to the DittoFS server.

The only metadata store type is badger (BadgerDB, embedded):
  --db-path:   Path to the BadgerDB directory (or prompted interactively)
  --in-memory: Keep the whole store in RAM instead; its contents are lost
               when the server stops. Meant for tests and throwaway servers.

Examples:
  # Add a BadgerDB store with flags
  dfsctl store metadata add --name persistent-meta --db-path /data/meta

  # Add a BadgerDB store interactively
  dfsctl store metadata add --name persistent-meta

  # Add an in-memory store
  dfsctl store metadata add --name scratch-meta --in-memory`,
	RunE: runAdd,
}

func init() {
	addCmd.Flags().StringVar(&addName, "name", "", "Store name (required)")
	addCmd.Flags().StringVar(&addType, "type", "badger", "Store type (only badger is supported)")
	addCmd.Flags().StringVar(&addConfig, "config", "", "Store configuration as JSON (for advanced config)")
	addCmd.Flags().StringVar(&addDBPath, "db-path", "", "BadgerDB directory path")
	addCmd.Flags().BoolVar(&addInMemory, "in-memory", false, "Keep the store in RAM only (contents lost on restart)")
	addCmd.MarkFlagsMutuallyExclusive("db-path", "in-memory")
	_ = addCmd.MarkFlagRequired("name")
}

func runAdd(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	// Build config based on type and flags
	config, err := buildMetadataConfig(addType, addConfig, addDBPath, addInMemory)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}

	req := &apiclient.CreateStoreRequest{
		Name:   addName,
		Type:   addType,
		Config: config,
	}

	store, err := client.CreateMetadataStore(req)
	if err != nil {
		return fmt.Errorf("failed to create metadata store: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, store, fmt.Sprintf("Metadata store '%s' (type: %s) created successfully", store.Name, store.Type))
}

func buildMetadataConfig(storeType, jsonConfig, dbPath string, inMemory bool) (any, error) {
	if storeType != "badger" {
		return nil, fmt.Errorf("unknown store type: %s (only badger is supported)", storeType)
	}

	// If JSON config is provided, use it directly
	if jsonConfig != "" {
		var config any
		if err := json.Unmarshal([]byte(jsonConfig), &config); err != nil {
			return nil, fmt.Errorf("invalid JSON config: %w", err)
		}
		return config, nil
	}

	if inMemory {
		return map[string]any{"in_memory": true}, nil
	}

	path := dbPath
	if path == "" {
		var err error
		path, err = prompt.InputRequired("Database path")
		if err != nil {
			return nil, err
		}
	}
	return map[string]any{"path": path}, nil
}
