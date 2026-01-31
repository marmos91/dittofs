package payload

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dittofsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	addName   string
	addType   string
	addConfig string
	// Filesystem specific
	addPath string
	// S3 specific
	addBucket    string
	addRegion    string
	addEndpoint  string
	addAccessKey string
	addSecretKey string
)

var addCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a payload store",
	Long: `Add a new payload store to the DittoFS server.

Supported types:
  - memory: In-memory store (fast, ephemeral)
  - filesystem: Local filesystem store
  - s3: AWS S3 or S3-compatible store

Type-specific options:
  filesystem:
    --path: Path to storage directory (or prompted interactively)

  s3:
    --bucket: S3 bucket name (or prompted interactively)
    --region: AWS region (default: us-east-1)
    --endpoint: Custom endpoint for S3-compatible stores
    --access-key: AWS access key ID
    --secret-key: AWS secret access key

Examples:
  # Add a memory store
  dittofsctl store payload add --name fast-content --type memory

  # Add a filesystem store with flags
  dittofsctl store payload add --name local-store --type filesystem --path /data/content

  # Add a filesystem store interactively
  dittofsctl store payload add --name local-store --type filesystem

  # Add an S3 store with flags
  dittofsctl store payload add --name s3-store --type s3 --bucket my-bucket --region us-west-2

  # Add an S3 store interactively
  dittofsctl store payload add --name s3-store --type s3

  # Add a MinIO store (S3-compatible)
  dittofsctl store payload add --name minio-store --type s3 --bucket data --endpoint http://localhost:9000`,
	RunE: runAdd,
}

func init() {
	addCmd.Flags().StringVar(&addName, "name", "", "Store name (required)")
	addCmd.Flags().StringVar(&addType, "type", "", "Store type: memory, filesystem, s3 (required)")
	addCmd.Flags().StringVar(&addConfig, "config", "", "Store configuration as JSON (for advanced config)")
	// Filesystem flags
	addCmd.Flags().StringVar(&addPath, "path", "", "Storage path (required for filesystem)")
	// S3 flags
	addCmd.Flags().StringVar(&addBucket, "bucket", "", "S3 bucket name (required for s3)")
	addCmd.Flags().StringVar(&addRegion, "region", "us-east-1", "AWS region (for s3)")
	addCmd.Flags().StringVar(&addEndpoint, "endpoint", "", "Custom S3 endpoint (for S3-compatible stores)")
	addCmd.Flags().StringVar(&addAccessKey, "access-key", "", "AWS access key ID (for s3)")
	addCmd.Flags().StringVar(&addSecretKey, "secret-key", "", "AWS secret access key (for s3)")
	_ = addCmd.MarkFlagRequired("name")
	_ = addCmd.MarkFlagRequired("type")
}

func runAdd(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	// Build config based on type and flags
	config, err := buildPayloadConfig(addType, addConfig, addPath, addBucket, addRegion, addEndpoint, addAccessKey, addSecretKey)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}

	req := &apiclient.CreateStoreRequest{
		Name:   addName,
		Type:   addType,
		Config: config,
	}

	store, err := client.CreatePayloadStore(req)
	if err != nil {
		return fmt.Errorf("failed to create payload store: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, store, fmt.Sprintf("Payload store '%s' (type: %s) created successfully", store.Name, store.Type))
}

func buildPayloadConfig(storeType, jsonConfig, path, bucket, region, endpoint, accessKey, secretKey string) (any, error) {
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

	case "filesystem":
		storagePath := path
		if storagePath == "" {
			var err error
			storagePath, err = prompt.InputRequired("Storage path")
			if err != nil {
				return nil, err
			}
		}
		return map[string]any{"path": storagePath}, nil

	case "s3":
		s3Bucket := bucket
		s3Region := region
		s3Endpoint := endpoint
		s3AccessKey := accessKey
		s3SecretKey := secretKey

		if s3Bucket == "" {
			var err error
			s3Bucket, err = prompt.InputRequired("S3 bucket name")
			if err != nil {
				return nil, err
			}

			s3Region, err = prompt.Input("AWS region", "us-east-1")
			if err != nil {
				return nil, err
			}

			s3Endpoint, err = prompt.InputOptional("Custom endpoint (for S3-compatible stores)")
			if err != nil {
				return nil, err
			}
		}

		// Prompt for credentials if not provided
		if s3AccessKey == "" {
			var err error
			s3AccessKey, err = prompt.InputOptional("Access key ID (leave empty for instance profile/env vars)")
			if err != nil {
				return nil, err
			}
		}

		if s3AccessKey != "" && s3SecretKey == "" {
			var err error
			s3SecretKey, err = prompt.Password("Secret access key")
			if err != nil {
				return nil, err
			}
		}

		config := map[string]any{
			"bucket": s3Bucket,
			"region": s3Region,
		}
		if s3Endpoint != "" {
			config["endpoint"] = s3Endpoint
		}
		if s3AccessKey != "" {
			config["access_key_id"] = s3AccessKey
		}
		if s3SecretKey != "" {
			config["secret_access_key"] = s3SecretKey
		}
		return config, nil

	default:
		return nil, fmt.Errorf("unknown store type: %s (supported: memory, filesystem, s3)", storeType)
	}
}
