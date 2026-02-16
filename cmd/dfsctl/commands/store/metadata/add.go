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
	addName   string
	addType   string
	addConfig string
	// BadgerDB specific
	addDBPath string
)

var addCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a metadata store",
	Long: `Add a new metadata store to the DittoFS server.

Supported types:
  - memory: In-memory store (fast, ephemeral)
  - badger: BadgerDB store (persistent, embedded)
  - postgres: PostgreSQL store (persistent, distributed)

Type-specific options:
  badger:
    --db-path: Path to BadgerDB directory (or prompted interactively)

  postgres:
    --config: JSON with connection settings, or omit for interactive prompts

Examples:
  # Add a memory store
  dfsctl store metadata add --name fast-meta --type memory

  # Add a BadgerDB store with flags
  dfsctl store metadata add --name persistent-meta --type badger --db-path /data/meta

  # Add a BadgerDB store interactively
  dfsctl store metadata add --name persistent-meta --type badger

  # Add a PostgreSQL store with JSON config
  dfsctl store metadata add --name pg-meta --type postgres --config '{"host":"localhost","dbname":"dittofs"}'

  # Add a PostgreSQL store interactively
  dfsctl store metadata add --name pg-meta --type postgres`,
	RunE: runAdd,
}

func init() {
	addCmd.Flags().StringVar(&addName, "name", "", "Store name (required)")
	addCmd.Flags().StringVar(&addType, "type", "", "Store type: memory, badger, postgres (required)")
	addCmd.Flags().StringVar(&addConfig, "config", "", "Store configuration as JSON (for advanced config)")
	addCmd.Flags().StringVar(&addDBPath, "db-path", "", "Database path (required for badger)")
	_ = addCmd.MarkFlagRequired("name")
	_ = addCmd.MarkFlagRequired("type")
}

func runAdd(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	// Build config based on type and flags
	config, err := buildMetadataConfig(addType, addConfig, addDBPath)
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

func buildMetadataConfig(storeType, jsonConfig, dbPath string) (any, error) {
	// If JSON config is provided, use it directly
	if jsonConfig != "" {
		var config any
		if err := json.Unmarshal([]byte(jsonConfig), &config); err != nil {
			return nil, fmt.Errorf("invalid JSON config: %w", err)
		}
		return config, nil
	}

	// Build config from type-specific flags or prompt interactively
	switch storeType {
	case "memory":
		return nil, nil

	case "badger":
		path := dbPath
		if path == "" {
			var err error
			path, err = prompt.InputRequired("Database path")
			if err != nil {
				return nil, err
			}
		}
		return map[string]any{"db_path": path}, nil

	case "postgres":
		host, err := prompt.Input("PostgreSQL host", "localhost")
		if err != nil {
			return nil, err
		}

		port, err := prompt.InputPort("PostgreSQL port", 5432)
		if err != nil {
			return nil, err
		}

		dbname, err := prompt.InputRequired("Database name")
		if err != nil {
			return nil, err
		}

		user, err := prompt.Input("Username", "postgres")
		if err != nil {
			return nil, err
		}

		password, err := prompt.Password("Password")
		if err != nil {
			return nil, err
		}

		sslmode, err := prompt.Input("SSL mode", "disable")
		if err != nil {
			return nil, err
		}

		return map[string]any{
			"host":     host,
			"port":     port,
			"dbname":   dbname,
			"user":     user,
			"password": password,
			"sslmode":  sslmode,
		}, nil

	default:
		return nil, fmt.Errorf("unknown store type: %s (supported: memory, badger, postgres)", storeType)
	}
}
