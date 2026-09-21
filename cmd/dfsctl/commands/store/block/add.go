package block

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	addName   string
	addType   string
	addConfig string
	// S3 specific
	addBucket          string
	addRegion          string
	addEndpoint        string
	addPrefix          string
	addAccessKey       string
	addSecretKey       string
	addCompression     string
	addParallelUploads int
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
	Short: "Add a block store",
	Long: `Add a new block store to the DittoFS server.

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

  Every s3 option left off the command line is asked for individually when
  stdin is a terminal. An empty endpoint means AWS S3; the resolved target is
  echoed before the store is created.

Examples:
  # Add an S3 store with flags
  dfsctl store block add --name s3-store --type s3 --bucket my-bucket --region us-west-2

  # Add an S3 store interactively
  dfsctl store block add --name s3-store --type s3

  # Add a MinIO store (S3-compatible)
  dfsctl store block add --name minio-store --type s3 --bucket data --endpoint http://localhost:9000

  # Add an S3 store with zstd block compression
  dfsctl store block add --name prod-s3 --type s3 --bucket my-bucket --compression zstd

  # Add a memory store (for testing)
  dfsctl store block add --name test-remote --type memory`,
	RunE: runAdd,
}

func init() {
	addCmd.Flags().StringVar(&addName, "name", "", "Store name (required)")
	addCmd.Flags().StringVar(&addType, "type", "s3", "Store type: s3, memory")
	addCmd.Flags().StringVar(&addConfig, "config", "", "Store configuration as JSON")
	// S3 flags
	addCmd.Flags().StringVar(&addBucket, "bucket", "", "S3 bucket name (required for s3)")
	addCmd.Flags().StringVar(&addRegion, "region", defaultS3Region, "AWS region (for s3)")
	addCmd.Flags().StringVar(&addEndpoint, "endpoint", "", "Custom S3 endpoint (for S3-compatible stores)")
	addCmd.Flags().StringVar(&addPrefix, "prefix", "", "Key prefix within the bucket (for s3)")
	addCmd.Flags().StringVar(&addAccessKey, "access-key", "", "AWS access key ID (for s3)")
	addCmd.Flags().StringVar(&addSecretKey, "secret-key", "", "AWS secret access key (for s3)")
	addCmd.Flags().StringVar(&addCompression, "compression", "", "Enable per-block compression: zstd, lz4 (default: off)")
	addCmd.Flags().IntVar(&addParallelUploads, "parallel-uploads", 0, "Max parallel chunk uploads to this remote (0 = adaptive: auto-tune to saturate the uplink)")
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

	enc := encryptionFlags{
		AEAD:       addEncryptionAEAD,
		KeyKind:    addEncryptionKeyKind,
		KeyFile:    addEncryptionKeyFile,
		KMIPHost:   addEncryptionKMIPHost,
		KMIPCA:     addEncryptionKMIPCA,
		KMIPCert:   addEncryptionKMIPCert,
		KMIPKey:    addEncryptionKMIPKey,
		KMIPKeyUID: addEncryptionKMIPKeyUID,
	}
	// Reject a malformed --compression or --encryption-* flag up front. Both
	// are validated again while the config is assembled; doing it before the
	// prompts means a typo surfaces immediately rather than after six answers.
	if _, err := buildCompressionBlock(addCompression); err != nil {
		return err
	}
	if _, err := buildEncryptionBlock(enc); err != nil {
		return err
	}

	s3 := s3Fields{
		bucket:    addBucket,
		region:    addRegion,
		endpoint:  addEndpoint,
		prefix:    addPrefix,
		accessKey: addAccessKey,
		secretKey: addSecretKey,
	}
	if addType == "s3" && addConfig == "" {
		if err := promptMissingS3Fields(&s3, cmd.Flags().Changed, terminalAsker()); err != nil {
			return cmdutil.HandleAbort(err)
		}
	}

	config, err := buildRemoteConfig(addType, addConfig, s3, addCompression, addParallelUploads, enc)
	if err != nil {
		return cmdutil.HandleAbort(err)
	}

	if s3Config, ok := config.(map[string]any); addType == "s3" && ok {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Target: %s, bucket %q, region %q\n",
			s3TargetDescription(s3Config),
			cmdutil.GetConfigString(s3Config, "bucket", ""),
			cmdutil.GetConfigString(s3Config, "region", ""))
	}

	req := &apiclient.CreateStoreRequest{
		Name:   addName,
		Type:   addType,
		Config: config,
	}

	store, err := client.CreateBlockStore(req)
	if err != nil {
		return fmt.Errorf("failed to create block store: %w", err)
	}

	return cmdutil.PrintResourceWithSuccess(os.Stdout, store, fmt.Sprintf("Block store '%s' (type: %s) created successfully", store.Name, store.Type))
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

// defaultS3Region is the region an s3 store falls back to when the operator
// names none: the --region default, and the answer its prompt offers.
const defaultS3Region = "us-east-1"

// s3Fields holds the settings of an s3 store, as supplied on the command line
// and then completed from the interactive prompts.
type s3Fields struct {
	bucket    string
	region    string
	endpoint  string
	prefix    string
	accessKey string
	secretKey string
}

// s3Asker asks the operator for one field at a time. It is a struct of
// functions rather than direct calls into the prompt package so the gating can
// be exercised without a terminal.
type s3Asker struct {
	required    func(label string) (string, error)
	optional    func(label string) (string, error)
	withDefault func(label, defaultValue string) (string, error)
	secret      func(label string) (string, error)
}

// terminalAsker returns the terminal-backed prompts, or nil when stdin is not
// a terminal. A session that cannot answer a question must fail on a missing
// value rather than block on a prompt nobody will ever see.
func terminalAsker() *s3Asker {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil
	}
	return &s3Asker{
		required:    prompt.InputRequired,
		optional:    prompt.InputOptional,
		withDefault: prompt.Input,
		secret:      func(label string) (string, error) { return prompt.PasswordWithValidation(label, 1) },
	}
}

// promptMissingS3Fields fills in every s3 setting the operator did not pass on
// the command line, one question per field. The fields are independent: naming
// one flag never suppresses another's question, so a run that passes only the
// bucket is still asked which endpoint to talk to — an unanswered endpoint
// means AWS, which is not a target anyone should reach by omission.
//
// supplied reports whether a flag was named, not whether it is non-empty, so
// an explicitly empty value is left alone. A nil ask means nobody is there to
// answer, and the required fields become an error instead.
func promptMissingS3Fields(f *s3Fields, supplied func(name string) bool, ask *s3Asker) error {
	if ask == nil {
		var missing []string
		for _, name := range []string{"bucket", "access-key", "secret-key"} {
			if !supplied(name) {
				missing = append(missing, "--"+name)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("%s required for an s3 store when stdin is not a terminal", strings.Join(missing, ", "))
		}
		return nil
	}

	var err error
	if !supplied("bucket") {
		if f.bucket, err = ask.required("S3 bucket name"); err != nil {
			return err
		}
	}
	if !supplied("region") {
		if f.region, err = ask.withDefault("AWS region", defaultS3Region); err != nil {
			return err
		}
	}
	if !supplied("prefix") {
		if f.prefix, err = ask.optional("Key prefix"); err != nil {
			return err
		}
	}
	if !supplied("endpoint") {
		if f.endpoint, err = ask.optional("Custom endpoint (empty for AWS)"); err != nil {
			return err
		}
	}
	if !supplied("access-key") {
		if f.accessKey, err = ask.required("Access key ID"); err != nil {
			return err
		}
	}
	if !supplied("secret-key") {
		if f.secretKey, err = ask.secret("Secret access key"); err != nil {
			return err
		}
	}
	return nil
}

// s3TargetDescription names the endpoint a store will actually talk to. An
// absent endpoint is AWS, which once the store is written is indistinguishable
// from a deliberately chosen gateway.
func s3TargetDescription(config map[string]any) string {
	if endpoint := cmdutil.GetConfigString(config, "endpoint", ""); endpoint != "" {
		return endpoint
	}
	return "AWS S3 (s3.amazonaws.com)"
}

func buildRemoteConfig(storeType, jsonConfig string, s3 s3Fields, compression string, parallelUploads int, enc encryptionFlags) (any, error) {
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
		if s3.region == "" {
			s3.region = defaultS3Region
		}
		config := map[string]any{
			"bucket":            s3.bucket,
			"region":            s3.region,
			"access_key_id":     s3.accessKey,
			"secret_access_key": s3.secretKey,
		}
		if s3.endpoint != "" {
			config["endpoint"] = s3.endpoint
		}
		if s3.prefix != "" {
			config["prefix"] = s3.prefix
		}
		if compressionBlock != nil {
			config["compression"] = compressionBlock
		}
		if encryptionBlock != nil {
			config["encryption"] = encryptionBlock
		}
		if parallelUploads > 0 {
			config["parallel_uploads"] = parallelUploads
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
