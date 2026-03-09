package remote

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
	// S3 specific
	addBucket    string
	addRegion    string
	addEndpoint  string
	addPrefix    string
	addAccessKey string
	addSecretKey string
)

var addCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a remote block store",
	Long: `Add a new remote block store to the DittoFS server.

Supported types:
  - s3: AWS S3 or S3-compatible store (durable, production)
  - memory: In-memory store (fast, ephemeral, for testing)

Type-specific options:
  s3:
    --bucket: S3 bucket name (or prompted interactively)
    --region: AWS region (default: us-east-1)
    --endpoint: Custom endpoint for S3-compatible stores
    --prefix: Key prefix within the bucket
    --access-key: AWS access key ID
    --secret-key: AWS secret access key

Examples:
  # Add an S3 store with flags
  dfsctl store block remote add --name s3-store --type s3 --bucket my-bucket --region us-west-2

  # Add an S3 store interactively
  dfsctl store block remote add --name s3-store --type s3

  # Add a MinIO store (S3-compatible)
  dfsctl store block remote add --name minio-store --type s3 --bucket data --endpoint http://localhost:9000

  # Add a memory store (for testing)
  dfsctl store block remote add --name test-remote --type memory`,
	RunE: runAdd,
}

func init() {
	addCmd.Flags().StringVar(&addName, "name", "", "Store name (required)")
	addCmd.Flags().StringVar(&addType, "type", "s3", "Store type: s3, memory")
	addCmd.Flags().StringVar(&addConfig, "config", "", "Store configuration as JSON")
	// S3 flags
	addCmd.Flags().StringVar(&addBucket, "bucket", "", "S3 bucket name (required for s3)")
	addCmd.Flags().StringVar(&addRegion, "region", "us-east-1", "AWS region (for s3)")
	addCmd.Flags().StringVar(&addEndpoint, "endpoint", "", "Custom S3 endpoint (for S3-compatible stores)")
	addCmd.Flags().StringVar(&addPrefix, "prefix", "", "Key prefix within the bucket (for s3)")
	addCmd.Flags().StringVar(&addAccessKey, "access-key", "", "AWS access key ID (for s3)")
	addCmd.Flags().StringVar(&addSecretKey, "secret-key", "", "AWS secret access key (for s3)")
	_ = addCmd.MarkFlagRequired("name")
}

func runAdd(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	config, err := buildRemoteConfig(addType, addConfig, addBucket, addRegion, addEndpoint, addPrefix, addAccessKey, addSecretKey)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}

	req := &apiclient.CreateStoreRequest{
		Name:   addName,
		Type:   addType,
		Config: config,
	}

	store, err := client.CreateBlockStore("remote", req)
	if err != nil {
		return fmt.Errorf("failed to create remote block store: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, store, fmt.Sprintf("Remote block store '%s' (type: %s) created successfully", store.Name, store.Type))
}

func buildRemoteConfig(storeType, jsonConfig, bucket, region, endpoint, prefix, accessKey, secretKey string) (any, error) {
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

	case "s3":
		s3Bucket := bucket
		s3Region := region
		s3Endpoint := endpoint
		s3Prefix := prefix
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

			s3Prefix, err = prompt.InputOptional("Key prefix")
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
		if s3Prefix != "" {
			config["prefix"] = s3Prefix
		}
		if s3AccessKey != "" {
			config["access_key_id"] = s3AccessKey
		}
		if s3SecretKey != "" {
			config["secret_access_key"] = s3SecretKey
		}
		return config, nil

	default:
		return nil, fmt.Errorf("unknown store type: %s (supported: s3, memory)", storeType)
	}
}
