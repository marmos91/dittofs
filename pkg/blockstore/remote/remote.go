package remote

import "context"

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

	// Close releases resources held by the store.
	Close() error

	// HealthCheck verifies the store is accessible.
	HealthCheck(ctx context.Context) error
}
