package remote

import (
	"context"

	"github.com/marmos91/dittofs/pkg/health"
)

// RemoteStore defines the interface for remote block storage backends.
// Blocks are immutable chunks of data stored with a string key.
//
// Key format: "{payloadID}/block-{blockIdx}"
// Example: "export/file.txt/block-0"
type RemoteStore interface {
	// WriteBlock writes a single block to storage.
	WriteBlock(ctx context.Context, blockKey string, data []byte) error

	// ReadBlock reads a complete block. Returns error if missing.
	ReadBlock(ctx context.Context, blockKey string) ([]byte, error)

	// ReadBlockRange reads a byte range from a block. Returns error if missing.
	ReadBlockRange(ctx context.Context, blockKey string, offset, length int64) ([]byte, error)

	// DeleteBlock removes a single block. Returns nil if missing.
	DeleteBlock(ctx context.Context, blockKey string) error

	// DeleteByPrefix removes all blocks matching the prefix.
	DeleteByPrefix(ctx context.Context, prefix string) error

	// ListByPrefix lists all block keys matching the prefix.
	ListByPrefix(ctx context.Context, prefix string) ([]string, error)

	// CopyBlock copies a block from srcKey to dstKey using server-side copy
	// when the backend supports it (e.g., S3 CopyObject). Falls back to
	// read-then-write for backends without native copy.
	// Returns blockstore.ErrBlockNotFound if the source block does not exist.
	CopyBlock(ctx context.Context, srcKey, dstKey string) error

	// Close releases resources held by the store.
	Close() error

	// HealthCheck verifies the store is accessible. This is the legacy
	// error-returning probe used internally by the syncer's HealthMonitor.
	// New callers should prefer Healthcheck (lowercase 'c') which returns
	// a structured [health.Report] and satisfies [health.Checker].
	HealthCheck(ctx context.Context) error

	// Healthcheck returns a structured health report and satisfies
	// [health.Checker]. Implementations typically delegate to HealthCheck
	// and wrap the result via [health.ReportFromError].
	Healthcheck(ctx context.Context) health.Report
}
