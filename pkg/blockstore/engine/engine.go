// Package engine provides the BlockStore orchestrator that composes local cache,
// remote store, and syncer into the blockstore.Store interface.
//
// The orchestrator lives in a sub-package (not the root blockstore package) to
// avoid import cycles: blockstore/local and blockstore/sync both import the root
// blockstore package for types and interfaces, so the root package cannot import
// them back.
package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	blocksync "github.com/marmos91/dittofs/pkg/blockstore/sync"
)

// Compile-time interface satisfaction check.
var _ blockstore.Store = (*BlockStore)(nil)

// Config holds the components that make up a BlockStore.
type Config struct {
	// Local is the on-node cache store (required).
	Local local.LocalStore

	// Remote is the durable backend store (nil for local-only mode).
	Remote remote.RemoteStore

	// Syncer handles async cache-to-remote transfers (required).
	Syncer *blocksync.Syncer
}

// BlockStore is the central orchestrator for block storage. It composes a local
// cache store, optional remote store, and syncer into the blockstore.Store
// interface. All protocol adapters and runtime code use BlockStore for I/O.
//
// Read operations check the local cache first, falling back to remote download
// via the syncer on cache miss. Write operations go directly to the local cache;
// the syncer handles background upload to remote.
type BlockStore struct {
	local  local.LocalStore
	remote remote.RemoteStore
	syncer *blocksync.Syncer
}

// New creates a new BlockStore from the given configuration.
// Local store and syncer are required; remote may be nil for local-only mode.
func New(cfg Config) (*BlockStore, error) {
	if cfg.Local == nil {
		return nil, errors.New("local store is required")
	}
	if cfg.Syncer == nil {
		return nil, errors.New("syncer is required")
	}

	return &BlockStore{
		local:  cfg.Local,
		remote: cfg.Remote,
		syncer: cfg.Syncer,
	}, nil
}

// Start initializes the store and starts background goroutines.
// Recovery runs on the local store first (if supported), then the syncer
// and local store background goroutines are started.
func (bs *BlockStore) Start(ctx context.Context) error {
	// Run recovery on local store if it supports it (FSStore has Recover).
	type recoverer interface {
		Recover(ctx context.Context) error
	}
	if r, ok := bs.local.(recoverer); ok {
		if err := r.Recover(ctx); err != nil {
			logger.Warn("BlockStore: local store recovery encountered errors", "error", err)
		}
	}

	// Start local store background goroutines (e.g., periodic FileBlock metadata persistence).
	// Use background context so these outlive the calling request context.
	bs.local.Start(context.Background())

	// Start syncer background goroutines (periodic uploader, transfer queue).
	bs.syncer.Start(context.Background())

	return nil
}

// Close releases resources held by the store. Stops syncer first (drains uploads),
// then closes local store and remote store.
func (bs *BlockStore) Close() error {
	var errs []error

	if err := bs.syncer.Close(); err != nil {
		errs = append(errs, fmt.Errorf("syncer close: %w", err))
	}
	if err := bs.local.Close(); err != nil {
		errs = append(errs, fmt.Errorf("local close: %w", err))
	}
	if bs.remote != nil {
		if err := bs.remote.Close(); err != nil {
			errs = append(errs, fmt.Errorf("remote close: %w", err))
		}
	}

	return errors.Join(errs...)
}

// ============================================================================
// Reader interface
// ============================================================================

// ReadAt reads data from storage at the given offset into dest.
// Checks local cache first, falling back to remote download on miss.
func (bs *BlockStore) ReadAt(ctx context.Context, payloadID string, data []byte, offset uint64) (int, error) {
	return bs.readAtInternal(ctx, payloadID, "", data, offset)
}

// ReadAtWithCOWSource reads data with copy-on-write source fallback.
// If data is not found in the primary payloadID, it falls back to cowSource.
func (bs *BlockStore) ReadAtWithCOWSource(ctx context.Context, payloadID, cowSource string, data []byte, offset uint64) (int, error) {
	return bs.readAtInternal(ctx, payloadID, cowSource, data, offset)
}

// GetSize returns the stored size of a payload.
// Checks local cache first, falls back to syncer (remote).
func (bs *BlockStore) GetSize(ctx context.Context, payloadID string) (uint64, error) {
	if size, found := bs.local.GetFileSize(ctx, payloadID); found {
		return size, nil
	}
	return bs.syncer.GetFileSize(ctx, payloadID)
}

// Exists checks whether a payload exists.
// Checks local cache first, falls back to syncer (remote).
func (bs *BlockStore) Exists(ctx context.Context, payloadID string) (bool, error) {
	if _, found := bs.local.GetFileSize(ctx, payloadID); found {
		return true, nil
	}
	return bs.syncer.Exists(ctx, payloadID)
}

// ============================================================================
// Writer interface
// ============================================================================

// WriteAt writes data to storage at the given offset.
// Writes go directly to local cache; the syncer handles background upload.
func (bs *BlockStore) WriteAt(ctx context.Context, payloadID string, data []byte, offset uint64) error {
	if len(data) == 0 {
		return nil
	}
	return bs.local.WriteAt(ctx, payloadID, data, offset)
}

// Truncate changes the size of a payload in both local cache and remote store.
func (bs *BlockStore) Truncate(ctx context.Context, payloadID string, newSize uint64) error {
	if err := bs.local.Truncate(ctx, payloadID, newSize); err != nil {
		return fmt.Errorf("local truncate failed: %w", err)
	}
	return bs.syncer.Truncate(ctx, payloadID, newSize)
}

// Delete removes all data for a payload from local cache and remote store.
func (bs *BlockStore) Delete(ctx context.Context, payloadID string) error {
	if err := bs.local.EvictMemory(ctx, payloadID); err != nil {
		return fmt.Errorf("local evict memory failed: %w", err)
	}
	return bs.syncer.Delete(ctx, payloadID)
}

// ============================================================================
// Flusher interface
// ============================================================================

// Flush ensures all dirty data for a payload is persisted.
func (bs *BlockStore) Flush(ctx context.Context, payloadID string) (*blockstore.FlushResult, error) {
	result, err := bs.syncer.Flush(ctx, payloadID)
	if err != nil {
		return nil, err
	}
	return &blockstore.FlushResult{Finalized: result.Finalized}, nil
}

// DrainAllUploads waits for all pending uploads to complete.
func (bs *BlockStore) DrainAllUploads(ctx context.Context) error {
	return bs.syncer.DrainAllUploads(ctx)
}

// ============================================================================
// Stats and Health
// ============================================================================

// Stats returns storage statistics from the local cache.
func (bs *BlockStore) Stats() (*blockstore.Stats, error) {
	localStats := bs.local.Stats()
	files := bs.local.ListFiles()
	return &blockstore.Stats{
		UsedSize:     0, // TODO: implement proper stats tracking
		ContentCount: uint64(len(files)),
		TotalSize:    uint64(localStats.MaxDisk),
	}, nil
}

// HealthCheck verifies the store is operational by checking the syncer health
// (which in turn checks the remote store).
func (bs *BlockStore) HealthCheck(ctx context.Context) error {
	return bs.syncer.HealthCheck(ctx)
}

// ============================================================================
// Accessors (for runtime/config wiring)
// ============================================================================

// Local returns the local store.
func (bs *BlockStore) Local() local.LocalStore { return bs.local }

// Remote returns the remote store (may be nil in local-only mode).
func (bs *BlockStore) Remote() remote.RemoteStore { return bs.remote }

// SyncerInstance returns the syncer.
func (bs *BlockStore) SyncerInstance() *blocksync.Syncer { return bs.syncer }

// ============================================================================
// Internal read logic
// ============================================================================

// readAtInternal reads from primary payloadID, falling back to cowSource on miss.
func (bs *BlockStore) readAtInternal(ctx context.Context, payloadID, cowSource string, data []byte, offset uint64) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	// Try primary cache first
	found, err := bs.local.ReadAt(ctx, payloadID, data, offset)
	if err != nil {
		return 0, fmt.Errorf("cache read failed: %w", err)
	}
	if found {
		return len(data), nil
	}

	if cowSource != "" {
		if err := bs.readFromCOWSource(ctx, payloadID, cowSource, data, offset); err != nil {
			return 0, err
		}
	} else {
		if err := bs.ensureAndReadFromCache(ctx, payloadID, data, offset); err != nil {
			return 0, err
		}
	}

	return len(data), nil
}

// readFromCOWSource reads from the COW source and copies data to primary cache.
func (bs *BlockStore) readFromCOWSource(ctx context.Context, payloadID, sourcePayloadID string, dest []byte, offset uint64) error {
	sourceFound, sourceErr := bs.local.ReadAt(ctx, sourcePayloadID, dest, offset)
	if sourceErr != nil {
		return fmt.Errorf("COW source read failed: %w", sourceErr)
	}

	if !sourceFound {
		err := bs.syncer.EnsureAvailable(ctx, sourcePayloadID, offset, uint32(len(dest)))
		if err != nil {
			return fmt.Errorf("ensure available for COW source failed: %w", err)
		}

		sourceFound, sourceErr = bs.local.ReadAt(ctx, sourcePayloadID, dest, offset)
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
	if err := bs.local.WriteAt(ctx, payloadID, dest, offset); err != nil {
		logger.Debug("COW cache write failed (non-fatal)", "payloadID", payloadID, "error", err)
	}

	return nil
}

// ensureAndReadFromCache downloads blocks from the store if needed and reads from cache.
func (bs *BlockStore) ensureAndReadFromCache(ctx context.Context, payloadID string, dest []byte, offset uint64) error {
	length := uint32(len(dest))

	// Fast path: direct-serve copies S3 data directly to dest, skipping a second ReadAt.
	filled, err := bs.syncer.EnsureAvailableAndRead(ctx, payloadID, offset, length, dest)
	if err != nil {
		return fmt.Errorf("direct download failed: %w", err)
	}
	if filled {
		return nil
	}

	found, err := bs.local.ReadAt(ctx, payloadID, dest, offset)
	if err != nil {
		return fmt.Errorf("read after download failed: %w", err)
	}
	if !found {
		clear(dest)
		logger.Debug("Sparse block: cache miss after download, returning zeros",
			"payloadID", payloadID)
	}

	return nil
}
