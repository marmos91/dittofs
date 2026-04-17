package repo

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	addName             string
	addKind             string
	addConfigJSON       string
	addSchedule         string
	addKeepCount        int
	addKeepAgeDays      int
	addEncryption       string // "on" | "off"
	addEncryptionKeyRef string

	// Local kind
	addPath string

	// S3 kind
	addBucket          string
	addRegion          string
	addEndpoint        string
	addPrefix          string
	addAccessKeyID     string
	addSecretAccessKey string
)

var addCmd = &cobra.Command{
	Use:   "add <store-name>",
	Short: "Add a backup repo to a metadata store",
	Long: `Add a new backup repo to a metadata store.

A backup repo declares where backups are stored (destination kind: local or s3),
an optional cron schedule, retention policy (count and/or age), and optional
encryption settings. The cron schedule is validated server-side (D-18) — this
CLI has no cron parser dependency.

S3 credentials (--access-key-id / --secret-access-key) are convenient for CI
but leave secrets in shell history; omit them to be prompted hidden (D-14).

Examples:
  # Interactive S3 repo
  dfsctl store metadata fast-meta repo add --name daily-s3 --kind s3

  # Scripted S3 repo with schedule + retention
  dfsctl store metadata fast-meta repo add \
    --name daily-s3 --kind s3 \
    --bucket my-backups --region us-east-1 \
    --access-key-id AKIA... --secret-access-key *** \
    --schedule "CRON_TZ=UTC 0 2 * * *" --keep-count 7

  # Local repo with encryption
  dfsctl store metadata fast-meta repo add \
    --name nightly-local --kind local --path /var/backups/dittofs \
    --encryption on --encryption-key-ref env:BACKUP_KEY

  # From JSON config (bypasses per-kind prompts)
  dfsctl store metadata fast-meta repo add \
    --name daily-s3 --kind s3 \
    --config @./repo.json`,
	Args: cobra.ExactArgs(1),
	RunE: runAdd,
}

func init() {
	addCmd.Flags().StringVar(&addName, "name", "", "Repo name (required)")
	addCmd.Flags().StringVar(&addKind, "kind", "", "Destination kind: local, s3 (required)")
	addCmd.Flags().StringVar(&addConfigJSON, "config", "", "Destination config as JSON (or @/path/to/file.json) — bypasses interactive prompts")
	addCmd.Flags().StringVar(&addSchedule, "schedule", "", `Cron schedule (optional; supports "CRON_TZ=UTC "-prefixed expressions; validated server-side)`)
	addCmd.Flags().IntVar(&addKeepCount, "keep-count", 0, "Retention count — keep last N successful backups (0 = no count policy)")
	addCmd.Flags().IntVar(&addKeepAgeDays, "keep-age-days", 0, "Retention age in days — keep backups newer than D days (0 = no age policy)")
	addCmd.Flags().StringVar(&addEncryption, "encryption", "", "Encryption: on or off (default: off)")
	addCmd.Flags().StringVar(&addEncryptionKeyRef, "encryption-key-ref", "", "env:VAR or file:/path — required when --encryption on")

	// Local kind
	addCmd.Flags().StringVar(&addPath, "path", "", "Local filesystem path (for kind=local; prompted if empty)")

	// S3 kind
	addCmd.Flags().StringVar(&addBucket, "bucket", "", "S3 bucket (for kind=s3)")
	addCmd.Flags().StringVar(&addRegion, "region", "", "S3 region (for kind=s3)")
	addCmd.Flags().StringVar(&addEndpoint, "endpoint", "", "S3 endpoint URL (optional; for S3-compatible backends)")
	addCmd.Flags().StringVar(&addPrefix, "prefix", "", "S3 key prefix (optional)")
	addCmd.Flags().StringVar(&addAccessKeyID, "access-key-id", "", "S3 access key ID (for kind=s3; prompted if empty)")
	addCmd.Flags().StringVar(&addSecretAccessKey, "secret-access-key", "", "S3 secret access key (for kind=s3; prompted hidden if empty)")

	_ = addCmd.MarkFlagRequired("name")
	_ = addCmd.MarkFlagRequired("kind")

	Cmd.AddCommand(addCmd)
}

// buildRepoConfig assembles the destination config map from flags or
// interactive prompts. When --config is set it wins and prompts are
// skipped (D-14 + scripting convention from metadata/add.go).
func buildRepoConfig(kind, jsonConfig, path, bucket, region, endpoint, prefix, accessKeyID, secretAccessKey string) (map[string]any, error) {
	if jsonConfig != "" {
		raw := jsonConfig
		if strings.HasPrefix(raw, "@") {
			data, err := os.ReadFile(raw[1:])
			if err != nil {
				return nil, fmt.Errorf("read config file: %w", err)
			}
			raw = string(data)
		}
		var cfg map[string]any
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return nil, fmt.Errorf("invalid JSON config: %w", err)
		}
		return cfg, nil
	}

	switch kind {
	case "local":
		p := path
		if p == "" {
			var err error
			p, err = prompt.InputRequired("Local backup path")
			if err != nil {
				return nil, cmdutil.HandleAbort(err)
			}
		}
		return map[string]any{"path": p}, nil
	case "s3":
		return buildS3RepoConfig(bucket, region, endpoint, prefix, accessKeyID, secretAccessKey)
	default:
		return nil, fmt.Errorf("unknown kind: %s (supported: local, s3)", kind)
	}
}

// buildS3RepoConfig follows the existing block/remote/add.go S3 prompt
// flow. D-15 research finding: the S3 block store (pkg/blockstore/remote/s3)
// REQUIRES explicit access_key_id + secret_access_key today (no ambient AWS
// chain — see s3.NewFromConfig). For consistency, backup-repo S3 config
// stores credentials the same way, prompted hidden when not provided.
func buildS3RepoConfig(bucket, region, endpoint, prefix, accessKeyID, secretAccessKey string) (map[string]any, error) {
	var err error

	// Interactive mode trigger: bucket unset means the operator is using
	// the prompt flow. In that case we prompt for the optional fields
	// too; when any of bucket/region is provided via flags we assume a
	// scripted run and leave optional fields as-is. This matches the
	// block/remote/add.go convention (lines 112-141).
	interactive := bucket == ""

	if interactive {
		bucket, err = prompt.InputRequired("S3 bucket")
		if err != nil {
			return nil, cmdutil.HandleAbort(err)
		}

		region, err = prompt.Input("AWS region", "us-east-1")
		if err != nil {
			return nil, cmdutil.HandleAbort(err)
		}

		endpoint, err = prompt.InputOptional("S3 endpoint URL")
		if err != nil {
			return nil, cmdutil.HandleAbort(err)
		}

		prefix, err = prompt.InputOptional("S3 key prefix")
		if err != nil {
			return nil, cmdutil.HandleAbort(err)
		}
	}

	// Default region when neither flag nor prompt provided one.
	if region == "" {
		region = "us-east-1"
	}

	// D-15: mirror pkg/blockstore/remote/s3 — credentials are required,
	// no ambient AWS chain. Match the block/remote/add.go prompt flow:
	// always prompt when the flag is empty, even in scripted mode, so
	// CI secret material can stay out of shell history.
	if accessKeyID == "" {
		accessKeyID, err = prompt.InputRequired("S3 access key ID")
		if err != nil {
			return nil, cmdutil.HandleAbort(err)
		}
	}
	if secretAccessKey == "" {
		secretAccessKey, err = prompt.PasswordWithValidation("S3 secret access key", 1)
		if err != nil {
			return nil, cmdutil.HandleAbort(err)
		}
	}

	cfg := map[string]any{
		"bucket":            bucket,
		"region":            region,
		"access_key_id":     accessKeyID,
		"secret_access_key": secretAccessKey,
	}
	if endpoint != "" {
		cfg["endpoint"] = endpoint
	}
	if prefix != "" {
		cfg["prefix"] = prefix
	}
	return cfg, nil
}

func runAdd(cmd *cobra.Command, args []string) error {
	storeName := args[0]

	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}

	req, err := buildAddRequest()
	if err != nil {
		return err
	}

	r, err := client.CreateBackupRepo(storeName, req)
	if err != nil {
		return fmt.Errorf("failed to create backup repo: %w", err)
	}
	return cmdutil.PrintResourceWithSuccess(
		os.Stdout,
		r,
		fmt.Sprintf("Backup repo '%s' created on store '%s'.", r.Name, storeName),
	)
}

// buildAddRequest assembles the BackupRepoRequest from the package-level
// flag state. Factored out so unit tests can exercise the validation
// logic without stubbing the HTTP client.
func buildAddRequest() (*apiclient.BackupRepoRequest, error) {
	cfg, err := buildRepoConfig(
		addKind, addConfigJSON,
		addPath,
		addBucket, addRegion, addEndpoint, addPrefix,
		addAccessKeyID, addSecretAccessKey,
	)
	if err != nil {
		return nil, err
	}

	req := &apiclient.BackupRepoRequest{
		Name:   addName,
		Kind:   addKind,
		Config: cfg,
	}
	if addSchedule != "" {
		req.Schedule = &addSchedule
	}
	if addKeepCount > 0 {
		req.KeepCount = &addKeepCount
	}
	if addKeepAgeDays > 0 {
		req.KeepAgeDays = &addKeepAgeDays
	}
	if addEncryption != "" {
		on := addEncryption == "on"
		if on && addEncryptionKeyRef == "" {
			return nil, fmt.Errorf("--encryption on requires --encryption-key-ref (env:KEY or file:/path)")
		}
		req.EncryptionEnabled = &on
	}
	if addEncryptionKeyRef != "" {
		req.EncryptionKeyRef = &addEncryptionKeyRef
	}

	return req, nil
}
