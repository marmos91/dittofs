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
	addName   string
	addType   string
	addConfig string
	addPath   string
)

var addCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a local block store",
	Long: `Add a new local block store to the DittoFS server.

Supported types:
  - fs: Filesystem-backed block store (fast, persistent)
  - memory: In-memory block store (fast, ephemeral, for testing)

Type-specific options:
  fs:
    --path: Block directory path (or prompted interactively)

Examples:
  # Add a filesystem block store
  dfsctl store block local add --name fs-cache --type fs --path /data/blocks

  # Add with JSON config
  dfsctl store block local add --name fs-cache --type fs --config '{"path":"/data/blocks"}'

  # Add a memory store (for testing)
  dfsctl store block local add --name test-local --type memory

  # Add interactively (prompts for path)
  dfsctl store block local add --name fs-cache --type fs`,
	RunE: runAdd,
}

func init() {
	addCmd.Flags().StringVar(&addName, "name", "", "Store name (required)")
	addCmd.Flags().StringVar(&addType, "type", "fs", "Store type: fs, memory")
	addCmd.Flags().StringVar(&addConfig, "config", "", "Store configuration as JSON")
	addCmd.Flags().StringVar(&addPath, "path", "", "Block directory path (for fs type)")
	_ = addCmd.MarkFlagRequired("name")
}

func runAdd(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	config, err := buildLocalConfig(addType, addConfig, addPath)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}

	req := &apiclient.CreateStoreRequest{
		Name:   addName,
		Type:   addType,
		Config: config,
	}

	store, err := client.CreateBlockStore("local", req)
	if err != nil {
		return fmt.Errorf("failed to create local block store: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, store, fmt.Sprintf("Local block store '%s' (type: %s) created successfully", store.Name, store.Type))
}

func buildLocalConfig(storeType, jsonConfig, path string) (any, error) {
	if jsonConfig != "" {
		var config any
		if err := json.Unmarshal([]byte(jsonConfig), &config); err != nil {
			return nil, fmt.Errorf("invalid JSON config: %w", err)
		}
		return config, nil
	}

	switch storeType {
	case "memory":
		return nil, nil

	case "fs":
		fsPath := path
		if fsPath == "" {
			var err error
			fsPath, err = prompt.InputRequired("Block directory path")
			if err != nil {
				return nil, err
			}
		}
		return map[string]any{
			"path": fsPath,
		}, nil

	default:
		return nil, fmt.Errorf("unknown store type: %s (supported: fs, memory)", storeType)
	}
}
