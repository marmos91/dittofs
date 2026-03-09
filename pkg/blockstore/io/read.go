package io

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore/local"
)

// SyncerReader provides download capabilities needed by read operations.
// This is a narrow interface satisfied by *sync.Syncer.
type SyncerReader interface {
	// EnsureAvailable ensures the requested data range is in cache,
	// downloading from remote if needed.
	EnsureAvailable(ctx context.Context, payloadID string, offset uint64, length uint32) error

	// EnsureAvailableAndRead downloads blocks and copies data directly to dest,
	// avoiding a second cache ReadAt. Returns (filled, error).
	EnsureAvailableAndRead(ctx context.Context, payloadID string, offset uint64, length uint32, dest []byte) (bool, error)
}

// ReadDeps contains the dependencies for read operations.
type ReadDeps struct {
	Local  local.LocalReader
	Writer local.LocalWriter // Used for COW write-back to primary cache
	Syncer SyncerReader
}

// ReadAt reads data at the specified offset from the local cache, falling back
// to remote download on cache miss.
func ReadAt(ctx context.Context, deps ReadDeps, payloadID string, data []byte, offset uint64) (int, error) {
	return readAtInternal(ctx, deps, payloadID, "", data, offset)
}

// ReadAtWithCOWSource reads data using a COW source for lazy copy.
// If data is not found in the primary payloadID, it falls back to cowSource.
func ReadAtWithCOWSource(ctx context.Context, deps ReadDeps, payloadID, cowSource string, data []byte, offset uint64) (int, error) {
	return readAtInternal(ctx, deps, payloadID, cowSource, data, offset)
}

// readAtInternal reads from primary payloadID, falling back to cowSource on miss.
func readAtInternal(ctx context.Context, deps ReadDeps, payloadID, cowSource string, data []byte, offset uint64) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	// Try primary cache first
	found, err := deps.Local.ReadAt(ctx, payloadID, data, offset)
	if err != nil {
		return 0, fmt.Errorf("cache read failed: %w", err)
	}
	if found {
		return len(data), nil
	}

	if cowSource != "" {
		if err := readFromCOWSource(ctx, deps, payloadID, cowSource, data, offset); err != nil {
			return 0, err
		}
	} else {
		if err := ensureAndReadFromCache(ctx, deps, payloadID, data, offset); err != nil {
			return 0, err
		}
	}

	return len(data), nil
}

// readFromCOWSource reads from the COW source and copies data to primary cache.
func readFromCOWSource(ctx context.Context, deps ReadDeps, payloadID, sourcePayloadID string, dest []byte, offset uint64) error {
	sourceFound, sourceErr := deps.Local.ReadAt(ctx, sourcePayloadID, dest, offset)
	if sourceErr != nil {
		return fmt.Errorf("COW source read failed: %w", sourceErr)
	}

	if !sourceFound {
		err := deps.Syncer.EnsureAvailable(ctx, sourcePayloadID, offset, uint32(len(dest)))
		if err != nil {
			return fmt.Errorf("ensure available for COW source failed: %w", err)
		}

		sourceFound, sourceErr = deps.Local.ReadAt(ctx, sourcePayloadID, dest, offset)
		if sourceErr != nil {
			return fmt.Errorf("COW source read after download failed: %w", sourceErr)
		}
		if !sourceFound {
			clear(dest)
			logger.Debug("Sparse COW block: returning zeros",
				"payloadID", sourcePayloadID)
		}
	}

	// Copy to primary cache for future reads (non-fatal if fails)
	if err := deps.Writer.WriteAt(ctx, payloadID, dest, offset); err != nil {
		logger.Debug("COW cache write failed (non-fatal)", "payloadID", payloadID, "error", err)
	}

	return nil
}

// ensureAndReadFromCache downloads blocks from the store if needed and reads from cache.
func ensureAndReadFromCache(ctx context.Context, deps ReadDeps, payloadID string, dest []byte, offset uint64) error {
	length := uint32(len(dest))

	// Fast path: direct-serve copies S3 data directly to dest, skipping a second ReadAt.
	filled, err := deps.Syncer.EnsureAvailableAndRead(ctx, payloadID, offset, length, dest)
	if err != nil {
		return fmt.Errorf("direct download failed: %w", err)
	}
	if filled {
		return nil
	}

	found, err := deps.Local.ReadAt(ctx, payloadID, dest, offset)
	if err != nil {
		return fmt.Errorf("read after download failed: %w", err)
	}
	if !found {
		// Sparse block: cache did not store the data. Explicitly zero dest
		// since it may be a sub-slice of the caller's buffer with stale data.
		clear(dest)
		logger.Debug("Sparse block: cache miss after download, returning zeros",
			"payloadID", payloadID)
	}

	return nil
}
