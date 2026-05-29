package blockstore

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	remotes3 "github.com/marmos91/dittofs/pkg/blockstore/remote/s3"
)

// Remote selectors. "memory" is the default (in-process); "s3"
// reads AWS_* env vars (see README) and validates credentials at
// construction time.
const (
	RemoteMemory = "memory"
	RemoteS3     = "s3"
)

// SetupRemote constructs the remote store named by opts.Remote.
// The returned cleanup is a no-op when the remote does not own
// shutdown resources (the engine Close also closes the remote, so
// callers that hand the store to NewEngine should NOT also call
// the cleanup); the raw-s3-put workload bypasses the engine and
// owns the cleanup itself.
func SetupRemote(ctx context.Context, opts Opts) (remote.RemoteStore, func(), error) {
	switch opts.Remote {
	case "", RemoteMemory:
		return remotememory.New(), func() {}, nil
	case RemoteS3:
		return setupS3Remote(ctx)
	default:
		return nil, nil, fmt.Errorf("unknown remote %q (want %q or %q)", opts.Remote, RemoteMemory, RemoteS3)
	}
}

// setupS3Remote builds an S3 remote from AWS_* env vars. Required:
// AWS_S3_BUCKET, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY. Optional:
// AWS_S3_REGION (default us-east-1), AWS_ENDPOINT_URL,
// AWS_S3_KEY_PREFIX, AWS_S3_MAX_RETRIES, AWS_S3_PATH_STYLE
// (default true when AWS_ENDPOINT_URL is set, false otherwise).
func setupS3Remote(ctx context.Context) (remote.RemoteStore, func(), error) {
	bucket := os.Getenv("AWS_S3_BUCKET")
	if bucket == "" {
		return nil, nil, fmt.Errorf("AWS_S3_BUCKET is required when remote=s3")
	}
	region := os.Getenv("AWS_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	endpoint := os.Getenv("AWS_ENDPOINT_URL")
	// Path style defaults to true for non-AWS endpoints (Localstack /
	// MinIO / DS3). Explicit AWS_S3_PATH_STYLE overrides the heuristic.
	pathStyle := endpoint != ""
	if v, ok := os.LookupEnv("AWS_S3_PATH_STYLE"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, nil, fmt.Errorf("AWS_S3_PATH_STYLE=%q: %w", v, err)
		}
		pathStyle = b
	}
	var maxRetries int
	if v, ok := os.LookupEnv("AWS_S3_MAX_RETRIES"); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, nil, fmt.Errorf("AWS_S3_MAX_RETRIES: want non-negative integer, got %q", v)
		}
		maxRetries = n
	}
	store, err := remotes3.NewFromConfig(ctx, remotes3.Config{
		Bucket:         bucket,
		Region:         region,
		Endpoint:       endpoint,
		AccessKey:      os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey:      os.Getenv("AWS_SECRET_ACCESS_KEY"),
		KeyPrefix:      os.Getenv("AWS_S3_KEY_PREFIX"),
		MaxRetries:     maxRetries,
		ForcePathStyle: pathStyle,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("s3.NewFromConfig: %w", err)
	}
	return store, func() { _ = store.Close() }, nil
}
