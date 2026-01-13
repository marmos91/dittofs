// Package block provides the block store interface for persistent storage.
package block

import (
	"context"
	"errors"
)

// BlockSize is the size of a single block (4MB).
const BlockSize = 4 * 1024 * 1024

// Common errors returned by BlockStore implementations.
var (
	// ErrBlockNotFound is returned when a requested block doesn't exist.
	ErrBlockNotFound = errors.New("block not found")

	// ErrStoreClosed is returned when operations are attempted on a closed store.
	ErrStoreClosed = errors.New("store is closed")
)

// Store defines the interface for block storage backends.
// Blocks are immutable 4MB chunks of data stored with a string key.
//
// Key format: "{shareName}/{contentID}/chunk-{chunkIdx}/block-{blockIdx}"
// Example: "archive/abc123/chunk-0/block-0"
type Store interface {
	// WriteBlock writes a single block to storage.
	// The block key uniquely identifies the block.
	// Data should be <= BlockSize (4MB).
	WriteBlock(ctx context.Context, blockKey string, data []byte) error

	// ReadBlock reads a complete block from storage.
	// Returns ErrBlockNotFound if the block doesn't exist.
	ReadBlock(ctx context.Context, blockKey string) ([]byte, error)

	// ReadBlockRange reads a byte range from a block.
	// This is more efficient than ReadBlock for partial reads.
	// Returns ErrBlockNotFound if the block doesn't exist.
	ReadBlockRange(ctx context.Context, blockKey string, offset, length int64) ([]byte, error)

	// DeleteBlock removes a single block from storage.
	// Returns nil if the block doesn't exist.
	DeleteBlock(ctx context.Context, blockKey string) error

	// DeleteByPrefix removes all blocks with a given prefix.
	// Use cases:
	// - DeleteByPrefix("shareName/contentID/") removes all blocks for a file
	// - DeleteByPrefix("shareName/") removes all blocks for a share
	DeleteByPrefix(ctx context.Context, prefix string) error

	// ListByPrefix lists all block keys with a given prefix.
	// Returns an empty slice if no blocks match.
	ListByPrefix(ctx context.Context, prefix string) ([]string, error)

	// Close releases any resources held by the store.
	Close() error

	// HealthCheck verifies the store is accessible and operational.
	// Returns nil if healthy, error describing the issue otherwise.
	HealthCheck(ctx context.Context) error
}

// BlockRef references a single block in storage.
type BlockRef struct {
	// Key is the full block key in storage.
	// Format: "{shareName}/{contentID}/chunk-{chunkIdx}/block-{blockIdx}"
	Key string

	// Size is the actual size of this block (may be < BlockSize for last block).
	Size uint32
}
