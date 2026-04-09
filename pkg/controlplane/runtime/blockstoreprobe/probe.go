// Package blockstoreprobe runs one-shot health probes against a
// [models.BlockStoreConfig] and converts the result into a
// [health.Report].
//
// The logic used to live inside the HTTP handler package
// (checkBlockStoreConfigHealth + helpers) where it served the legacy
// /health route. Moving it here keeps the probe pure and callable from
// both the legacy handler path and the new runtime cached-checker layer
// (internal/controlplane/runtime.Runtime.BlockStoreChecker), without
// dragging an HTTP handler import into pkg/controlplane/runtime.
//
// The behaviour is identical to the prior handler-local implementation:
//
//   - local/memory → always healthy (in-memory store).
//   - local/fs → stat the configured path, require it to be a
//     writable directory, round-trip a zero-byte tempfile.
//   - remote/memory → always healthy (in-memory store).
//   - remote/s3 → instantiate an s3 client from the same fields the
//     handler used and call HealthCheck on it.
//
// On any failure the returned Report carries [health.StatusUnhealthy]
// with a short human-readable Message. Successes carry
// [health.StatusHealthy] and a short explanatory Message. CheckedAt
// and LatencyMs are always populated.
package blockstoreprobe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore/remote/s3"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/health"
)

// Probe runs a one-shot health check against the given block store
// config and returns a [health.Report]. The caller owns the context:
// the probe aborts when ctx is canceled and surfaces the cancellation
// as [health.StatusUnhealthy] via the underlying IO call's error.
//
// A nil bs returns [health.StatusUnhealthy] with an explanatory
// message rather than panicking.
func Probe(ctx context.Context, bs *models.BlockStoreConfig) health.Report {
	start := time.Now()
	finish := func(status health.Status, msg string) health.Report {
		end := time.Now()
		return health.Report{
			Status:    status,
			Message:   msg,
			CheckedAt: end.UTC(),
			LatencyMs: end.Sub(start).Milliseconds(),
		}
	}

	if bs == nil {
		return finish(health.StatusUnhealthy, "block store config is nil")
	}

	statusOf := func(ok bool) health.Status {
		if ok {
			return health.StatusHealthy
		}
		return health.StatusUnhealthy
	}

	switch bs.Kind {
	case models.BlockStoreKindLocal:
		status, msg := probeLocal(ctx, bs)
		return finish(status, msg)
	case models.BlockStoreKindRemote:
		ok, msg := probeRemote(ctx, bs)
		return finish(statusOf(ok), msg)
	default:
		return finish(health.StatusUnhealthy, fmt.Sprintf("unknown block store kind: %s", bs.Kind))
	}
}

// probeLocal preserves the previous checkLocalBlockStoreHealth behaviour:
// fs stores must expose a writable directory at the configured path,
// memory stores are always healthy. The caller's context is checked
// before the expensive filesystem operations so cancellation during a
// long-running Stat aborts the remaining write probe cleanly.
//
// The return is a [health.Status] (not a plain bool) so the tempfile
// cleanup path can downgrade to [health.StatusDegraded] — the write
// itself succeeded, so the store is usable, but a failed cleanup
// leaves a dot-file behind and is worth surfacing to operators.
func probeLocal(ctx context.Context, bs *models.BlockStoreConfig) (health.Status, string) {
	if err := ctx.Err(); err != nil {
		return health.StatusUnhealthy, "context canceled: " + err.Error()
	}
	if bs.Type == "memory" {
		return health.StatusHealthy, "in-memory store is always healthy"
	}
	if bs.Type != "fs" {
		return health.StatusUnhealthy, fmt.Sprintf("unknown local store type: %s", bs.Type)
	}

	config, err := bs.GetConfig()
	if err != nil {
		return health.StatusUnhealthy, "failed to parse store configuration"
	}

	rawPath, _ := config["path"].(string)
	if rawPath == "" {
		return health.StatusUnhealthy, "no path configured"
	}
	path := filepath.Clean(rawPath)

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return health.StatusUnhealthy, "configured path does not exist"
		}
		return health.StatusUnhealthy, "cannot access configured path"
	}
	if !info.IsDir() {
		return health.StatusUnhealthy, "configured path is not a directory"
	}

	// Re-check cancellation between the Stat call and the write probe:
	// a slow/hung Stat followed by caller cancellation should not
	// proceed to create (and leak) a tempfile.
	if err := ctx.Err(); err != nil {
		return health.StatusUnhealthy, "context canceled: " + err.Error()
	}

	f, err := os.CreateTemp(path, ".dfs-health-check-*")
	if err != nil {
		return health.StatusUnhealthy, "configured path is not writable"
	}
	name := f.Name()
	if closeErr := f.Close(); closeErr != nil {
		// Try to clean up even on close failure, but the write probe
		// itself is now inconclusive — report unhealthy so operators
		// investigate.
		_ = os.Remove(name)
		return health.StatusUnhealthy, "write probe close failed: " + closeErr.Error()
	}
	if removeErr := os.Remove(name); removeErr != nil && !os.IsNotExist(removeErr) {
		// Write + close succeeded so the store is functionally usable;
		// cleanup failure is a leak-risk, not a correctness failure.
		// Degraded lets dashboards flag it without tripping outage
		// alerts.
		return health.StatusDegraded, "write probe cleanup failed: " + removeErr.Error()
	}

	return health.StatusHealthy, "path accessible and writable"
}

// probeRemote preserves the previous checkRemoteBlockStoreHealth
// behaviour: s3 stores are probed by constructing a temporary client
// and calling its HealthCheck method; memory stores are always healthy.
func probeRemote(ctx context.Context, bs *models.BlockStoreConfig) (bool, string) {
	if bs.Type == "memory" {
		return true, "in-memory store is always healthy"
	}
	if bs.Type != "s3" {
		return false, fmt.Sprintf("unknown remote store type: %s", bs.Type)
	}

	config, err := bs.GetConfig()
	if err != nil {
		return false, "failed to parse store configuration"
	}

	bucket, _ := config["bucket"].(string)
	if bucket == "" {
		return false, "no bucket configured"
	}

	region := "us-east-1"
	if r, ok := config["region"].(string); ok && r != "" {
		region = r
	}

	endpoint, _ := config["endpoint"].(string)
	accessKey, _ := config["access_key_id"].(string)
	secretKey, _ := config["secret_access_key"].(string)
	forcePathStyle, hasPathStyle := config["force_path_style"].(bool)
	if endpoint != "" && !hasPathStyle {
		forcePathStyle = true
	}

	remoteStore, err := s3.NewFromConfig(ctx, s3.Config{
		Bucket:         bucket,
		Region:         region,
		Endpoint:       endpoint,
		AccessKey:      accessKey,
		SecretKey:      secretKey,
		ForcePathStyle: forcePathStyle,
	})
	if err != nil {
		return false, "failed to initialize S3 client"
	}
	defer func() { _ = remoteStore.Close() }()

	if err := remoteStore.HealthCheck(ctx); err != nil {
		return false, "S3 connectivity check failed"
	}

	return true, fmt.Sprintf("S3 bucket accessible: %s (region: %s)", bucket, region)
}
