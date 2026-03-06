// Package store provides the block store interface for persistent storage.
package store

import (
	"context"
	"errors"
)

// BlockSize is the size of a single block (8MB).
const BlockSize = 8 * 1024 * 1024

// Common errors returned by BlockStore implementations.
var (
	// ErrBlockNotFound is returned when a requested block doesn't exist.
	ErrBlockNotFound = errors.New("block not found")

	// ErrStoreClosed is returned when operations are attempted on a closed store.
	ErrStoreClosed = errors.New("store is closed")
)

// BlockStore defines the interface for block storage backends.
// Blocks are immutable chunks of data (up to BlockSize) stored with a string key.
//
// Key format: "{payloadID}/block-{blockIdx}"
// Example: "export/file.txt/block-0"
type BlockStore interface {
	// WriteBlock writes a single block to storage.
	WriteBlock(ctx context.Context, blockKey string, data []byte) error

	// ReadBlock reads a complete block. Returns ErrBlockNotFound if missing.
	ReadBlock(ctx context.Context, blockKey string) ([]byte, error)

	// ReadBlockRange reads a byte range from a block. Returns ErrBlockNotFound if missing.
	ReadBlockRange(ctx context.Context, blockKey string, offset, length int64) ([]byte, error)

	// DeleteBlock removes a single block. Returns nil if missing.
	DeleteBlock(ctx context.Context, blockKey string) error

	// DeleteByPrefix removes all blocks matching the prefix.
	DeleteByPrefix(ctx context.Context, prefix string) error

	// ListByPrefix lists all block keys matching the prefix.
	ListByPrefix(ctx context.Context, prefix string) ([]string, error)

	Close() error

	// HealthCheck verifies the store is accessible.
	HealthCheck(ctx context.Context) error
}

// BlockRef references a single block in storage.
type BlockRef struct {
	// Key is the full block key in storage.
	// Format: "{payloadID}/block-{blockIdx}"
	Key string

	// Size is the actual size of this block (may be < BlockSize for last block).
	Size uint32
}
