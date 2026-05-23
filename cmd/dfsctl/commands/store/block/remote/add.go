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
	addBucket      string
	addRegion      string
	addEndpoint    string
	addPrefix      string
	addAccessKey   string
	addSecretKey   string
	addCompression string
	// Encryption (client-side, optional)
	addEncryptionAEAD       string
	addEncryptionKeyKind    string
	addEncryptionKeyFile    string
	addEncryptionKMIPHost   string
	addEncryptionKMIPCA     string
	addEncryptionKMIPCert   string
	addEncryptionKMIPKey    string
	addEncryptionKMIPKeyUID string
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

  # Add an S3 store with zstd block compression
  dfsctl store block remote add --name prod-s3 --type s3 --bucket my-bucket --compression zstd

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
	addCmd.Flags().StringVar(&addCompression, "compression", "", "Enable per-block compression: zstd, lz4 (default: off)")
	// Encryption flags
	addCmd.Flags().StringVar(&addEncryptionAEAD, "encryption-aead", "", "Enable client-side encryption with the given AEAD: aes-256-gcm, chacha20-poly1305, xchacha20-poly1305")
	addCmd.Flags().StringVar(&addEncryptionKeyKind, "encryption-key-kind", "", "Key provider: local | kmip (required when --encryption-aead is set)")
	addCmd.Flags().StringVar(&addEncryptionKeyFile, "encryption-key-file", "", "Path to local key file (kind=local)")
	addCmd.Flags().StringVar(&addEncryptionKMIPHost, "encryption-kmip-endpoint", "", "KMIP server endpoint host:port (kind=kmip)")
	addCmd.Flags().StringVar(&addEncryptionKMIPCA, "encryption-kmip-ca", "", "KMIP server CA bundle (kind=kmip, optional)")
	addCmd.Flags().StringVar(&addEncryptionKMIPCert, "encryption-kmip-cert", "", "KMIP client certificate (kind=kmip)")
	addCmd.Flags().StringVar(&addEncryptionKMIPKey, "encryption-kmip-key", "", "KMIP client private key (kind=kmip)")
	addCmd.Flags().StringVar(&addEncryptionKMIPKeyUID, "encryption-kmip-key-uid", "", "KMIP managed symmetric key UID (kind=kmip)")
	_ = addCmd.MarkFlagRequired("name")
}

func runAdd(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	config, err := buildRemoteConfig(addType, addConfig, addBucket, addRegion, addEndpoint, addPrefix, addAccessKey, addSecretKey, addCompression, encryptionFlags{
		AEAD:       addEncryptionAEAD,
		KeyKind:    addEncryptionKeyKind,
		KeyFile:    addEncryptionKeyFile,
		KMIPHost:   addEncryptionKMIPHost,
		KMIPCA:     addEncryptionKMIPCA,
		KMIPCert:   addEncryptionKMIPCert,
		KMIPKey:    addEncryptionKMIPKey,
		KMIPKeyUID: addEncryptionKMIPKeyUID,
	})
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

type encryptionFlags struct {
	AEAD       string
	KeyKind    string
	KeyFile    string
	KMIPHost   string
	KMIPCA     string
	KMIPCert   string
	KMIPKey    string
	KMIPKeyUID string
}

func buildRemoteConfig(storeType, jsonConfig, bucket, region, endpoint, prefix, accessKey, secretKey, compression string, enc encryptionFlags) (any, error) {
	if jsonConfig != "" {
		var config any
		if err := json.Unmarshal([]byte(jsonConfig), &config); err != nil {
			return nil, fmt.Errorf("invalid JSON config: %w", err)
		}
		return config, nil
	}

	compressionBlock, err := buildCompressionBlock(compression)
	if err != nil {
		return nil, err
	}
	encryptionBlock, err := buildEncryptionBlock(enc)
	if err != nil {
		return nil, err
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

		if s3AccessKey == "" {
			var err error
			s3AccessKey, err = prompt.InputRequired("Access key ID")
			if err != nil {
				return nil, err
			}
		}
		if s3SecretKey == "" {
			var err error
			s3SecretKey, err = prompt.PasswordWithValidation("Secret access key", 1)
			if err != nil {
				return nil, err
			}
		}

		config := map[string]any{
			"bucket":            s3Bucket,
			"region":            s3Region,
			"access_key_id":     s3AccessKey,
			"secret_access_key": s3SecretKey,
		}
		if s3Endpoint != "" {
			config["endpoint"] = s3Endpoint
		}
		if s3Prefix != "" {
			config["prefix"] = s3Prefix
		}
		if compressionBlock != nil {
			config["compression"] = compressionBlock
		}
		if encryptionBlock != nil {
			config["encryption"] = encryptionBlock
		}
		return config, nil

	default:
		return nil, fmt.Errorf("unknown store type: %s (supported: s3, memory)", storeType)
	}
}

// buildEncryptionBlock validates the --encryption-* flags and returns
// the JSON sub-object to merge into the remote config under the
// "encryption" key. Returns (nil, nil) when --encryption-aead is empty
// (encryption off).
func buildEncryptionBlock(f encryptionFlags) (map[string]any, error) {
	if f.AEAD == "" {
		// All other --encryption-* flags require --encryption-aead. Fail
		// loud rather than silently dropping the operator's intent.
		if f.KeyKind != "" || f.KeyFile != "" || f.KMIPHost != "" || f.KMIPKeyUID != "" {
			return nil, fmt.Errorf("--encryption-aead is required when any --encryption-* flag is set")
		}
		return nil, nil
	}
	switch f.AEAD {
	case "aes-256-gcm", "chacha20-poly1305", "xchacha20-poly1305":
	default:
		return nil, fmt.Errorf("invalid --encryption-aead value %q (want one of: aes-256-gcm, chacha20-poly1305, xchacha20-poly1305)", f.AEAD)
	}
	key, err := buildEncryptionKeyBlock(f)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"aead": f.AEAD,
		"key":  key,
	}, nil
}

func buildEncryptionKeyBlock(f encryptionFlags) (map[string]any, error) {
	switch f.KeyKind {
	case "local":
		if f.KeyFile == "" {
			return nil, fmt.Errorf("--encryption-key-file is required for --encryption-key-kind=local")
		}
		return map[string]any{
			"kind": "local",
			"file": f.KeyFile,
		}, nil
	case "kmip":
		if f.KMIPHost == "" || f.KMIPCert == "" || f.KMIPKey == "" || f.KMIPKeyUID == "" {
			return nil, fmt.Errorf("--encryption-kmip-endpoint, --encryption-kmip-cert, --encryption-kmip-key, --encryption-kmip-key-uid are required for --encryption-key-kind=kmip")
		}
		out := map[string]any{
			"kind":        "kmip",
			"endpoint":    f.KMIPHost,
			"client_cert": f.KMIPCert,
			"client_key":  f.KMIPKey,
			"key_uid":     f.KMIPKeyUID,
		}
		if f.KMIPCA != "" {
			out["server_ca"] = f.KMIPCA
		}
		return out, nil
	case "":
		return nil, fmt.Errorf("--encryption-key-kind is required when --encryption-aead is set (want: local, kmip)")
	default:
		return nil, fmt.Errorf("invalid --encryption-key-kind %q (want: local, kmip)", f.KeyKind)
	}
}

// buildCompressionBlock validates the --compression flag and returns the
// JSON sub-object to merge into the remote config map. Returns (nil, nil)
// when the flag is empty (compression off).
func buildCompressionBlock(algo string) (map[string]any, error) {
	switch algo {
	case "":
		return nil, nil
	case "zstd", "lz4":
		return map[string]any{"algo": algo}, nil
	default:
		return nil, fmt.Errorf("invalid --compression value %q (want one of: zstd, lz4)", algo)
	}
}
