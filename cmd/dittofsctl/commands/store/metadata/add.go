package metadata

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	addName   string
	addType   string
	addConfig string
)

var addCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a metadata store",
	Long: `Add a new metadata store to the DittoFS server.

Supported types:
  - memory: In-memory store (fast, ephemeral)
  - badger: BadgerDB store (persistent, embedded)
  - postgres: PostgreSQL store (persistent, distributed)

Examples:
  # Add a memory store
  dittofsctl store metadata add --name fast-meta --type memory

  # Add a BadgerDB store with config
  dittofsctl store metadata add --name persistent-meta --type badger --config '{"db_path":"/data/meta"}'`,
	RunE: runAdd,
}

func init() {
	addCmd.Flags().StringVar(&addName, "name", "", "Store name (required)")
	addCmd.Flags().StringVar(&addType, "type", "", "Store type: memory, badger, postgres (required)")
	addCmd.Flags().StringVar(&addConfig, "config", "", "Store configuration as JSON")
	_ = addCmd.MarkFlagRequired("name")
	_ = addCmd.MarkFlagRequired("type")
}

func runAdd(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	var config any
	if addConfig != "" {
		if err := json.Unmarshal([]byte(addConfig), &config); err != nil {
			return fmt.Errorf("invalid JSON config: %w", err)
		}
	}

	req := &apiclient.CreateMetadataStoreRequest{
		Name:   addName,
		Type:   addType,
		Config: config,
	}

	store, err := client.CreateMetadataStore(req)
	if err != nil {
		return fmt.Errorf("failed to create metadata store: %w", err)
	}

	return cmdutil.PrintCreatedResource(os.Stdout, store, fmt.Sprintf("Metadata store '%s' (type: %s) created successfully", store.Name, store.Type))
}
