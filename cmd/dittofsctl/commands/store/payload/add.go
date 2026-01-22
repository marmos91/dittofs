package payload

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
	Short: "Add a payload store",
	Long: `Add a new payload store to the DittoFS server.

Supported types:
  - memory: In-memory store (fast, ephemeral)
  - filesystem: Local filesystem store
  - s3: AWS S3 or S3-compatible store

Examples:
  # Add a memory store
  dittofsctl store payload add --name fast-content --type memory

  # Add a filesystem store
  dittofsctl store payload add --name local-store --type filesystem --config '{"path":"/data/content"}'

  # Add an S3 store
  dittofsctl store payload add --name s3-store --type s3 --config '{"bucket":"my-bucket","region":"us-east-1"}'`,
	RunE: runAdd,
}

func init() {
	addCmd.Flags().StringVar(&addName, "name", "", "Store name (required)")
	addCmd.Flags().StringVar(&addType, "type", "", "Store type: memory, filesystem, s3 (required)")
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

	req := &apiclient.CreatePayloadStoreRequest{
		Name:   addName,
		Type:   addType,
		Config: config,
	}

	store, err := client.CreatePayloadStore(req)
	if err != nil {
		return fmt.Errorf("failed to create payload store: %w", err)
	}

	return cmdutil.PrintCreatedResource(os.Stdout, store, fmt.Sprintf("Payload store '%s' (type: %s) created successfully", store.Name, store.Type))
}
