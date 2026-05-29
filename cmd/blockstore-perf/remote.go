package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	remotes3 "github.com/marmos91/dittofs/pkg/blockstore/remote/s3"
)

// setupRemote constructs the RemoteStore backing the harness run.
// Returns a cleanup that closes the store ONLY for memory remotes —
// the engine.Store Close already closes the remote it owns, so
// when the remote is passed into engine.New the caller must skip our
// cleanup (the s3 path is only used by the raw-s3-put workload that
// bypasses the engine, and that path takes ownership of the cleanup).
func setupRemote(cfg config) (remote.RemoteStore, func(), error) {
	switch cfg.remote {
	case "memory":
		return remotememory.New(), func() {}, nil
	case "s3":
		return setupS3Remote()
	default:
		return nil, nil, fmt.Errorf("unknown --remote %q (want memory|s3)", cfg.remote)
	}
}

func setupS3Remote() (remote.RemoteStore, func(), error) {
	bucket := os.Getenv("AWS_S3_BUCKET")
	if bucket == "" {
		return nil, nil, fmt.Errorf("AWS_S3_BUCKET is required when --remote=s3")
	}
	region := os.Getenv("AWS_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	endpoint := os.Getenv("AWS_ENDPOINT_URL")
	// Default to path-style for non-AWS endpoints (Localstack/MinIO/DS3).
	// Explicit AWS_S3_PATH_STYLE overrides the heuristic.
	pathStyle := endpoint != ""
	if v, ok := os.LookupEnv("AWS_S3_PATH_STYLE"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, nil, fmt.Errorf("AWS_S3_PATH_STYLE: %w", err)
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
	store, err := remotes3.NewFromConfig(context.Background(), remotes3.Config{
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
