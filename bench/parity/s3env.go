package parity

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/marmos91/dittofs/pkg/block/remote"
	remotes3 "github.com/marmos91/dittofs/pkg/block/remote/s3"
)

// s3Config is the shared S3 target both tools point at. Populated from
// environment variables ONLY — credentials are never read from or written to
// files, and only the endpoint host (never keys) appears in artifacts.
//
// Required: AWS_S3_BUCKET, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY.
// Optional: AWS_ENDPOINT_URL (MinIO/Cubbit/SCW; empty = AWS),
// AWS_S3_REGION (default us-east-1), AWS_S3_PATH_STYLE (default true when an
// endpoint is set), AWS_S3_KEY_PREFIX (root prefix inside the bucket).
type s3Config struct {
	Bucket    string
	Region    string
	Endpoint  string
	AccessKey string
	SecretKey string
	KeyPrefix string
	PathStyle bool
}

// s3ConfigFromEnv reads the target from the environment, failing fast with a
// message that names every missing variable.
func s3ConfigFromEnv() (*s3Config, error) {
	cfg := &s3Config{
		Bucket:    os.Getenv("AWS_S3_BUCKET"),
		Region:    os.Getenv("AWS_S3_REGION"),
		Endpoint:  os.Getenv("AWS_ENDPOINT_URL"),
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		KeyPrefix: os.Getenv("AWS_S3_KEY_PREFIX"),
	}
	var missing []string
	for _, v := range []struct{ name, val string }{
		{"AWS_S3_BUCKET", cfg.Bucket},
		{"AWS_ACCESS_KEY_ID", cfg.AccessKey},
		{"AWS_SECRET_ACCESS_KEY", cfg.SecretKey},
	} {
		if v.val == "" {
			missing = append(missing, v.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s "+
			"(credentials are env-only by design — never put them in files)",
			strings.Join(missing, ", "))
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	cfg.PathStyle = cfg.Endpoint != ""
	if v, ok := os.LookupEnv("AWS_S3_PATH_STYLE"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("AWS_S3_PATH_STYLE=%q: %w", v, err)
		}
		cfg.PathStyle = b
	}
	return cfg, nil
}

// endpointHost returns the endpoint host for artifacts ("aws" when empty).
func (c *s3Config) endpointHost() string {
	if c.Endpoint == "" {
		return "aws"
	}
	if u, err := url.Parse(c.Endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return c.Endpoint
}

// prefixRoot returns the configured root key prefix with a trailing slash,
// or "" when unset.
func (c *s3Config) prefixRoot() string {
	if c.KeyPrefix == "" {
		return ""
	}
	return strings.TrimSuffix(c.KeyPrefix, "/") + "/"
}

// newRemote constructs a dittofs S3 remote store rooted at prefix.
func (c *s3Config) newRemote(ctx context.Context, prefix string) (remote.RemoteStore, error) {
	store, err := remotes3.NewFromConfig(ctx, remotes3.Config{
		Bucket:         c.Bucket,
		Region:         c.Region,
		Endpoint:       c.Endpoint,
		AccessKey:      c.AccessKey,
		SecretKey:      c.SecretKey,
		KeyPrefix:      prefix,
		ForcePathStyle: c.PathStyle,
	})
	if err != nil {
		return nil, fmt.Errorf("s3.NewFromConfig: %w", err)
	}
	return store, nil
}

// rcloneEnv returns the process environment for rclone invocations, carrying
// the same target as an env-only remote named "parity" (no config file, no
// creds on disk or argv).
func (c *s3Config) rcloneEnv() []string {
	env := append(os.Environ(),
		"RCLONE_CONFIG_PARITY_TYPE=s3",
		"RCLONE_CONFIG_PARITY_PROVIDER=Other",
		"RCLONE_CONFIG_PARITY_ACCESS_KEY_ID="+c.AccessKey,
		"RCLONE_CONFIG_PARITY_SECRET_ACCESS_KEY="+c.SecretKey,
		"RCLONE_CONFIG_PARITY_REGION="+c.Region,
	)
	if c.Endpoint != "" {
		env = append(env, "RCLONE_CONFIG_PARITY_ENDPOINT="+c.Endpoint)
	}
	if c.PathStyle {
		env = append(env, "RCLONE_CONFIG_PARITY_FORCE_PATH_STYLE=true")
	}
	return env
}

// rcloneTarget renders the rclone path for a key prefix inside the bucket.
func (c *s3Config) rcloneTarget(prefix string) string {
	return "parity:" + c.Bucket + "/" + prefix
}

// gitCommit best-effort resolves the current commit for the artifact header.
func gitCommit() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
