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
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local"
	"github.com/marmos91/dittofs/pkg/blockstore/readcache"
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

	// ReadCacheBytes is the memory budget for the L1 read cache per share.
	// 0 disables L1 caching. Passed directly to readcache.New as byte budget.
	ReadCacheBytes int64

	// PrefetchWorkers is the number of goroutines for sequential prefetch.
	// 0 disables prefetching.
	PrefetchWorkers int
}

// BlockStore is the central orchestrator for block storage. It composes a local
// cache store, optional remote store, and syncer into the blockstore.Store
// interface. All protocol adapters and runtime code use BlockStore for I/O.
//
// Read operations check the L1 read cache first, then the local cache, falling
// back to remote download via the syncer on cache miss. Write operations go
// directly to the local cache and invalidate L1; the syncer handles background
// upload to remote.
type BlockStore struct {
	local  local.LocalStore
	remote remote.RemoteStore
	syncer *blocksync.Syncer

	readCache  *readcache.ReadCache  // nil when disabled (ReadCacheBytes=0)
	prefetcher *readcache.Prefetcher // nil when disabled (PrefetchWorkers=0 or readCache nil)

	prefetchWorkers int // stored from config, used in Start()
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
		local:           cfg.Local,
		remote:          cfg.Remote,
		syncer:          cfg.Syncer,
		readCache:       readcache.New(cfg.ReadCacheBytes),
		prefetchWorkers: cfg.PrefetchWorkers,
	}, nil
}

// Start initializes the store and starts background goroutines.
// Recovery runs on the local store first (if supported), then the syncer
// and local store background goroutines are started. Finally, the prefetcher
// is created if both L1 cache and prefetch workers are configured.
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

	// Create prefetcher if L1 cache is enabled and workers are configured.
	// Created in Start() (not New()) because the loadBlock closure captures bs,
	// and NewPrefetcher starts workers immediately.
	if bs.readCache != nil && bs.prefetchWorkers > 0 {
		bs.prefetcher = readcache.NewPrefetcher(
			bs.prefetchWorkers,
			bs.readCache,
			bs.loadBlock,
			bs.local,
		)
		bs.readCache.SetPrefetcher(bs.prefetcher)
	}

	return nil
}

// loadBlock loads a single block from local store, falling back to remote via syncer.
// Used by the prefetcher to fill L1 cache with upcoming blocks.
func (bs *BlockStore) loadBlock(ctx context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error) {
	data, dataSize, err := bs.local.GetBlockData(ctx, payloadID, blockIdx)
	if err == nil {
		return data, dataSize, nil
	}

	// Fall back to syncer for remote download.
	offset := blockIdx * uint64(blockstore.BlockSize)
	if syncErr := bs.syncer.EnsureAvailable(ctx, payloadID, offset, uint32(blockstore.BlockSize)); syncErr != nil {
		return nil, 0, syncErr
	}

	return bs.local.GetBlockData(ctx, payloadID, blockIdx)
}

// Close releases resources held by the store. Closes prefetcher first (stops workers),
// then read cache, then syncer (drains uploads), local store, and remote store.
func (bs *BlockStore) Close() error {
	// Prefetcher and ReadCache are nil-safe (handle nil receiver).
	bs.prefetcher.Close()
	bs.readCache.Close()

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

// ReadAt reads data from storage at the given offset into dest.
// Checks L1 read cache first, then local cache, falling back to remote download on miss.
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

// WriteAt writes data to storage at the given offset.
// Writes go directly to local cache; the syncer handles background upload.
// L1 cache entries for affected blocks are invalidated and prefetcher is reset.
func (bs *BlockStore) WriteAt(ctx context.Context, payloadID string, data []byte, offset uint64) error {
	if len(data) == 0 {
		return nil
	}
	if err := bs.local.WriteAt(ctx, payloadID, data, offset); err != nil {
		return err
	}
	bs.readCache.InvalidateRange(payloadID, offset, len(data), blockstore.BlockSize)
	return nil
}

// Truncate changes the size of a payload in both local cache and remote store.
// Invalidates L1 entries above the new size and resets prefetcher state.
func (bs *BlockStore) Truncate(ctx context.Context, payloadID string, newSize uint64) error {
	if err := bs.local.Truncate(ctx, payloadID, newSize); err != nil {
		return fmt.Errorf("local truncate failed: %w", err)
	}

	bs.readCache.InvalidateAboveSize(payloadID, newSize, blockstore.BlockSize)

	return bs.syncer.Truncate(ctx, payloadID, newSize)
}

// Delete removes all data for a payload from local cache and remote store.
// Invalidates all L1 entries for the file and resets prefetcher state.
func (bs *BlockStore) Delete(ctx context.Context, payloadID string) error {
	if err := bs.local.EvictMemory(ctx, payloadID); err != nil {
		return fmt.Errorf("local evict memory failed: %w", err)
	}
	bs.readCache.InvalidateAndReset(payloadID)
	return bs.syncer.Delete(ctx, payloadID)
}

// Flush ensures all dirty data for a payload is persisted.
// After flush, auto-promotes block data into L1 cache if the file fits within
// the L1 budget (data is in OS page cache, so the read is essentially free).
func (bs *BlockStore) Flush(ctx context.Context, payloadID string) (*blockstore.FlushResult, error) {
	result, err := bs.syncer.Flush(ctx, payloadID)
	if err != nil {
		return result, err
	}

	// Auto-promote flushed blocks into L1 (skip files larger than L1 budget).
	// MaxBytes() returns 0 when readCache is nil, so the size check fails naturally.
	if l1Budget := bs.readCache.MaxBytes(); l1Budget > 0 {
		size, found := bs.local.GetFileSize(ctx, payloadID)
		if found && size > 0 && int64(size) <= l1Budget {
			bs.readCache.FillFromStore(ctx, payloadID, 0, size, blockstore.BlockSize, bs.local.GetBlockData)
		}
	}

	return result, nil
}

// DrainAllUploads waits for all pending uploads to complete.
func (bs *BlockStore) DrainAllUploads(ctx context.Context) error {
	return bs.syncer.DrainAllUploads(ctx)
}

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

// RemoteForTesting returns the remote store for cross-package test verification
// (e.g., shared remote store identity). Do not use in production code.
func (bs *BlockStore) RemoteForTesting() remote.RemoteStore { return bs.remote }

// ListFiles returns the payloadIDs of all files tracked in the local cache.
func (bs *BlockStore) ListFiles() []string { return bs.local.ListFiles() }

// EvictLocal removes all cached data (memory and disk) for a file.
func (bs *BlockStore) EvictLocal(ctx context.Context, payloadID string) error {
	if err := bs.local.EvictMemory(ctx, payloadID); err != nil {
		return err
	}
	return bs.local.DeleteAllBlockFiles(ctx, payloadID)
}

// LocalStats returns a snapshot of local cache statistics.
func (bs *BlockStore) LocalStats() local.Stats { return bs.local.Stats() }

// CacheStats holds comprehensive cache statistics for a BlockStore.
// BlocksDirty is populated from the local store's in-memory dirty block count.
// BlocksLocal/BlocksRemote/BlocksTotal require metadata store queries and are
// populated by the aggregation layer (cache handler), not by the engine directly.
type CacheStats struct {
	FileCount    int `json:"file_count"`
	BlocksDirty  int `json:"blocks_dirty"`
	BlocksLocal  int `json:"blocks_local"`
	BlocksRemote int `json:"blocks_remote"`
	BlocksTotal  int `json:"blocks_total"`

	LocalDiskUsed int64 `json:"local_disk_used"`
	LocalDiskMax  int64 `json:"local_disk_max"`
	LocalMemUsed  int64 `json:"local_mem_used"`
	LocalMemMax   int64 `json:"local_mem_max"`

	L1Entries  int   `json:"l1_entries"`
	L1CurBytes int64 `json:"l1_cur_bytes"`
	L1MaxBytes int64 `json:"l1_max_bytes"`

	HasRemote      bool `json:"has_remote"`
	PendingSyncs   int  `json:"pending_syncs"`
	PendingUploads int  `json:"pending_uploads"`
	CompletedSyncs int  `json:"completed_syncs"`
	FailedSyncs    int  `json:"failed_syncs"`
}

// GetCacheStats returns comprehensive cache statistics.
func (bs *BlockStore) GetCacheStats() CacheStats {
	localStats := bs.local.Stats()
	files := bs.local.ListFiles()

	l1Stats := bs.readCache.Stats()

	pending, completed, failed := bs.syncer.Queue().Stats()
	_, uploads, _ := bs.syncer.Queue().PendingByType()

	return CacheStats{
		FileCount:      len(files),
		BlocksDirty:    localStats.MemBlockCount,
		LocalDiskUsed:  localStats.DiskUsed,
		LocalDiskMax:   localStats.MaxDisk,
		LocalMemUsed:   localStats.MemUsed,
		LocalMemMax:    localStats.MaxMemory,
		L1Entries:      l1Stats.Entries,
		L1CurBytes:     l1Stats.CurBytes,
		L1MaxBytes:     l1Stats.MaxBytes,
		HasRemote:      bs.remote != nil,
		PendingSyncs:   pending,
		PendingUploads: uploads,
		CompletedSyncs: completed,
		FailedSyncs:    failed,
	}
}

// EvictL1Cache clears all entries from the L1 read cache.
// Returns the number of entries that were cleared.
func (bs *BlockStore) EvictL1Cache() int {
	entries := bs.readCache.Stats().Entries // nil-safe: returns zero
	bs.readCache.Close()                    // nil-safe: no-op
	return entries
}

// HasRemoteStore returns true if this BlockStore has a remote store configured.
func (bs *BlockStore) HasRemoteStore() bool {
	return bs.remote != nil
}

// SetRetentionPolicy updates the retention policy on the underlying local store.
// Delegates to the local store's SetRetentionPolicy method.
func (bs *BlockStore) SetRetentionPolicy(policy blockstore.RetentionPolicy, ttl time.Duration) {
	bs.local.SetRetentionPolicy(policy, ttl)
}

// SetEvictionEnabled controls whether the local store can evict blocks to free disk space.
// Delegates to the local store's SetEvictionEnabled method.
func (bs *BlockStore) SetEvictionEnabled(enabled bool) {
	bs.local.SetEvictionEnabled(enabled)
}

// readAtInternal reads from primary payloadID, falling back to cowSource on miss.
// When L1 cache is enabled, checks L1 first and fills L1 after successful read.
func (bs *BlockStore) readAtInternal(ctx context.Context, payloadID, cowSource string, data []byte, offset uint64) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	isPrimaryRead := cowSource == ""

	// L1 fast path: try to serve entirely from L1 cache.
	// Only for primary reads (no COW source) with L1 enabled.
	if bs.readCache != nil && isPrimaryRead {
		if n, ok := bs.tryL1Read(payloadID, data, offset); ok {
			bs.readCache.NotifyRead(payloadID, offset, uint64(len(data)), blockstore.BlockSize)
			return n, nil
		}
	}

	// Try primary local cache.
	found, err := bs.local.ReadAt(ctx, payloadID, data, offset)
	if err != nil {
		return 0, fmt.Errorf("cache read failed: %w", err)
	}
	if found {
		if isPrimaryRead {
			bs.promoteToL1(ctx, payloadID, offset, uint64(len(data)))
		}
		return len(data), nil
	}

	if !isPrimaryRead {
		if err := bs.readFromCOWSource(ctx, payloadID, cowSource, data, offset); err != nil {
			return 0, err
		}
		return len(data), nil
	}

	if err := bs.ensureAndReadFromCache(ctx, payloadID, data, offset); err != nil {
		return 0, err
	}
	bs.promoteToL1(ctx, payloadID, offset, uint64(len(data)))

	return len(data), nil
}

// promoteToL1 fills the L1 read cache from the local store for the given byte
// range and notifies the prefetcher about the read. Both calls are nil-safe
// (no-op when L1 is disabled).
func (bs *BlockStore) promoteToL1(ctx context.Context, payloadID string, offset, length uint64) {
	bs.readCache.FillFromStore(ctx, payloadID, offset, length, blockstore.BlockSize, bs.local.GetBlockData)
	bs.readCache.NotifyRead(payloadID, offset, length, blockstore.BlockSize)
}

// tryL1Read attempts to serve a read entirely from L1 cache.
// Returns (bytesRead, true) if all blocks in the range were in L1.
// Returns (0, false) if any block was missing or returned fewer bytes than needed.
func (bs *BlockStore) tryL1Read(payloadID string, data []byte, offset uint64) (int, bool) {
	startBlock := offset / blockstore.BlockSize
	endBlock := (offset + uint64(len(data)) - 1) / blockstore.BlockSize

	for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
		blockStart := blockIdx * blockstore.BlockSize
		blockOff := uint32(0)
		if offset > blockStart {
			blockOff = uint32(offset - blockStart)
		}
		destOff := uint64(0)
		if blockStart > offset {
			destOff = blockStart - offset
		}
		remaining := uint64(len(data)) - destOff
		if remaining == 0 {
			break
		}

		// Limit to what fits in this block starting at blockOff.
		readLen := min(remaining, blockstore.BlockSize-uint64(blockOff))

		buf := data[destOff : destOff+readLen]
		n, hit := bs.readCache.Get(payloadID, blockIdx, buf, blockOff)
		if !hit || uint64(n) != readLen {
			return 0, false
		}
	}

	return len(data), true
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
