// Package engine provides the BlockStore orchestrator that composes local store,
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
	"github.com/marmos91/dittofs/pkg/blockstore/readbuffer"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	blocksync "github.com/marmos91/dittofs/pkg/blockstore/sync"
	"github.com/marmos91/dittofs/pkg/health"
)

// Compile-time interface satisfaction check.
var _ blockstore.Store = (*BlockStore)(nil)

// Config holds the components that make up a BlockStore.
type Config struct {
	// Local is the on-node block store (required).
	Local local.LocalStore

	// Remote is the durable backend store (nil for local-only mode).
	Remote remote.RemoteStore

	// Syncer handles async local-to-remote transfers (required).
	Syncer *blocksync.Syncer

	// FileBlockStore provides block metadata for block store statistics.
	// When set, GetStats() populates BlocksLocal/BlocksRemote/BlocksTotal.
	FileBlockStore blockstore.FileBlockStore

	// ReadBufferBytes is the memory budget for the read buffer per share.
	// 0 disables the read buffer. Passed directly to readbuffer.New as byte budget.
	ReadBufferBytes int64

	// PrefetchWorkers is the number of goroutines for sequential prefetch.
	// 0 disables prefetching.
	PrefetchWorkers int
}

// BlockStore is the central orchestrator for block storage. It composes a local
// store, optional remote store, and syncer into the blockstore.Store
// interface. All protocol adapters and runtime code use BlockStore for I/O.
//
// Read operations check the read buffer first, then the local store, falling
// back to remote download via the syncer on miss. Write operations go
// directly to the local store and invalidate the read buffer; the syncer
// handles background upload to remote.
type BlockStore struct {
	local  local.LocalStore
	remote remote.RemoteStore
	syncer *blocksync.Syncer

	fileBlockStore blockstore.FileBlockStore // optional: for block count stats

	readBuffer *readbuffer.ReadBuffer // nil when disabled (ReadBufferBytes=0)
	prefetcher *readbuffer.Prefetcher // nil when disabled (PrefetchWorkers=0 or readBuffer nil)

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
		fileBlockStore:  cfg.FileBlockStore,
		readBuffer:      readbuffer.New(cfg.ReadBufferBytes),
		prefetchWorkers: cfg.PrefetchWorkers,
	}, nil
}

// Start initializes the store and starts background goroutines.
// Recovery runs on the local store first (if supported), then the syncer
// and local store background goroutines are started. Finally, the prefetcher
// is created if both the read buffer and prefetch workers are configured.
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

	// Wire health callback to toggle eviction on remote health changes.
	// When remote goes unhealthy, suspend eviction to prevent evicting blocks
	// that cannot be re-downloaded. When healthy again, re-enable eviction.
	bs.syncer.SetHealthCallback(func(healthy bool) {
		bs.local.SetEvictionEnabled(healthy)
		if healthy {
			logger.Info("Remote store healthy: eviction re-enabled")
		} else {
			logger.Warn("Remote store unhealthy: eviction suspended")
		}
	})

	// Create prefetcher if read buffer is enabled and workers are configured.
	// Created in Start() (not New()) because the loadBlock closure captures bs,
	// and NewPrefetcher starts workers immediately.
	if bs.readBuffer != nil && bs.prefetchWorkers > 0 {
		bs.prefetcher = readbuffer.NewPrefetcher(
			bs.prefetchWorkers,
			bs.readBuffer,
			bs.loadBlock,
			bs.local,
		)
		bs.readBuffer.SetPrefetcher(bs.prefetcher)
	}

	return nil
}

// loadBlock loads a single block from local store, falling back to remote via syncer.
// Used by the prefetcher to fill the read buffer with upcoming blocks.
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
// then read buffer, then syncer (drains uploads), local store, and remote store.
func (bs *BlockStore) Close() error {
	// Prefetcher and ReadBuffer are nil-safe (handle nil receiver).
	bs.prefetcher.Close()
	bs.readBuffer.Close()

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
// Checks read buffer first, then local store, falling back to remote download on miss.
func (bs *BlockStore) ReadAt(ctx context.Context, payloadID string, data []byte, offset uint64) (int, error) {
	return bs.readAtInternal(ctx, payloadID, "", data, offset)
}

// ReadAtWithCOWSource reads data with copy-on-write source fallback.
// If data is not found in the primary payloadID, it falls back to cowSource.
func (bs *BlockStore) ReadAtWithCOWSource(ctx context.Context, payloadID, cowSource string, data []byte, offset uint64) (int, error) {
	return bs.readAtInternal(ctx, payloadID, cowSource, data, offset)
}

// GetSize returns the stored size of a payload.
// Checks local store first, falls back to syncer (remote).
func (bs *BlockStore) GetSize(ctx context.Context, payloadID string) (uint64, error) {
	if size, found := bs.local.GetFileSize(ctx, payloadID); found {
		return size, nil
	}
	return bs.syncer.GetFileSize(ctx, payloadID)
}

// Exists checks whether a payload exists.
// Checks local store first, falls back to syncer (remote).
func (bs *BlockStore) Exists(ctx context.Context, payloadID string) (bool, error) {
	if _, found := bs.local.GetFileSize(ctx, payloadID); found {
		return true, nil
	}
	return bs.syncer.Exists(ctx, payloadID)
}

// WriteAt writes data to storage at the given offset.
// Writes go directly to the local store; the syncer handles background upload.
// Read buffer entries for affected blocks are invalidated and prefetcher is reset.
func (bs *BlockStore) WriteAt(ctx context.Context, payloadID string, data []byte, offset uint64) error {
	if len(data) == 0 {
		return nil
	}
	if err := bs.local.WriteAt(ctx, payloadID, data, offset); err != nil {
		return err
	}
	bs.readBuffer.InvalidateRange(payloadID, offset, len(data), blockstore.BlockSize)
	return nil
}

// Truncate changes the size of a payload in both local store and remote store.
// Invalidates read buffer entries above the new size and resets prefetcher state.
func (bs *BlockStore) Truncate(ctx context.Context, payloadID string, newSize uint64) error {
	if err := bs.local.Truncate(ctx, payloadID, newSize); err != nil {
		return fmt.Errorf("local truncate failed: %w", err)
	}

	bs.readBuffer.InvalidateAboveSize(payloadID, newSize, blockstore.BlockSize)

	return bs.syncer.Truncate(ctx, payloadID, newSize)
}

// Delete removes all data for a payload from local store and remote store.
// Invalidates all read buffer entries for the file and resets prefetcher state.
func (bs *BlockStore) Delete(ctx context.Context, payloadID string) error {
	if err := bs.local.EvictMemory(ctx, payloadID); err != nil {
		return fmt.Errorf("local evict memory failed: %w", err)
	}
	bs.readBuffer.InvalidateAndReset(payloadID)
	return bs.syncer.Delete(ctx, payloadID)
}

// Flush ensures all dirty data for a payload is persisted.
// After flush, auto-promotes block data into the read buffer if the file fits
// within the budget (data is in OS page cache, so the read is essentially free).
func (bs *BlockStore) Flush(ctx context.Context, payloadID string) (*blockstore.FlushResult, error) {
	result, err := bs.syncer.Flush(ctx, payloadID)
	if err != nil {
		return result, err
	}

	// Auto-promote flushed blocks into read buffer (skip files larger than budget).
	// MaxBytes() returns 0 when readBuffer is nil, so the size check fails naturally.
	if rbBudget := bs.readBuffer.MaxBytes(); rbBudget > 0 {
		size, found := bs.local.GetFileSize(ctx, payloadID)
		if found && size > 0 && int64(size) <= rbBudget {
			bs.readBuffer.FillFromStore(ctx, payloadID, 0, size, blockstore.BlockSize, bs.local.GetBlockData)
		}
	}

	return result, nil
}

// DrainAllUploads waits for all pending uploads to complete.
func (bs *BlockStore) DrainAllUploads(ctx context.Context) error {
	return bs.syncer.DrainAllUploads(ctx)
}

// Stats returns storage statistics from the local store.
func (bs *BlockStore) Stats() (*blockstore.Stats, error) {
	localStats := bs.local.Stats()
	files := bs.local.ListFiles()
	used := uint64(localStats.DiskUsed)
	total := uint64(localStats.MaxDisk)
	avail := uint64(0)
	if total > used {
		avail = total - used
	}
	count := uint64(len(files))
	avg := uint64(0)
	if count > 0 {
		avg = used / count
	}
	return &blockstore.Stats{
		UsedSize:      used,
		ContentCount:  count,
		TotalSize:     total,
		AvailableSize: avail,
		AverageSize:   avg,
	}, nil
}

// HealthCheck verifies the store is operational by checking the syncer health
// (which in turn checks the remote store).
//
// Legacy error-returning probe. New callers should prefer Healthcheck
// (lowercase 'c') which returns a structured [health.Report] derived
// from both the local and remote stores and satisfies [health.Checker].
func (bs *BlockStore) HealthCheck(ctx context.Context) error {
	return bs.syncer.HealthCheck(ctx)
}

// Healthcheck returns the engine's overall health, computed as the
// worst-of of its underlying local and remote stores. The result
// satisfies [health.Checker] so the API layer can wrap the engine in
// a [health.CachedChecker] for /status routes.
//
// Derivation rules (worst-of):
//
//   - If the local store reports unhealthy → engine is unhealthy
//     (we can't even serve cached blocks).
//   - If a remote store is configured and reports unhealthy → engine
//     is degraded (local reads still work, but new uploads will queue
//     and the system is operating in offline-write mode).
//   - Otherwise → healthy.
//
// The combined message preserves the worst-status component's message
// so operators can see exactly which subsystem is at fault.
func (bs *BlockStore) Healthcheck(ctx context.Context) health.Report {
	start := time.Now()

	if err := ctx.Err(); err != nil {
		return health.NewUnhealthyReport(err.Error(), time.Since(start))
	}

	localRep := bs.local.Healthcheck(ctx)
	if localRep.Status == health.StatusUnhealthy {
		return health.NewUnhealthyReport("local: "+localRep.Message, time.Since(start))
	}

	if bs.remote != nil {
		remoteRep := bs.remote.Healthcheck(ctx)
		if remoteRep.Status == health.StatusUnhealthy {
			// Local works, remote is unreachable: degraded — reads
			// still served from local cache, writes will queue.
			return health.Report{
				Status:    health.StatusDegraded,
				Message:   "remote unreachable: " + remoteRep.Message,
				CheckedAt: time.Now().UTC(),
				LatencyMs: time.Since(start).Milliseconds(),
			}
		}
	}

	return health.NewHealthyReport(time.Since(start))
}

// RemoteForTesting returns the remote store for cross-package test verification
// (e.g., shared remote store identity). Do not use in production code.
func (bs *BlockStore) RemoteForTesting() remote.RemoteStore { return bs.remote }

// ListFiles returns the payloadIDs of all files tracked in the local store.
func (bs *BlockStore) ListFiles() []string { return bs.local.ListFiles() }

// EvictLocal removes all local data (memory and disk) for a file.
func (bs *BlockStore) EvictLocal(ctx context.Context, payloadID string) error {
	if err := bs.local.EvictMemory(ctx, payloadID); err != nil {
		return err
	}
	return bs.local.DeleteAllBlockFiles(ctx, payloadID)
}

// LocalStats returns a snapshot of local store statistics.
func (bs *BlockStore) LocalStats() local.Stats { return bs.local.Stats() }

// BlockStoreStats holds comprehensive block store statistics for a BlockStore.
type BlockStoreStats struct {
	FileCount    int `json:"file_count"`
	BlocksDirty  int `json:"blocks_dirty"`
	BlocksLocal  int `json:"blocks_local"`
	BlocksRemote int `json:"blocks_remote"`
	BlocksTotal  int `json:"blocks_total"`

	LocalDiskUsed int64 `json:"local_disk_used"`
	LocalDiskMax  int64 `json:"local_disk_max"`
	LocalMemUsed  int64 `json:"local_mem_used"`
	LocalMemMax   int64 `json:"local_mem_max"`

	ReadBufferEntries int   `json:"read_buffer_entries"`
	ReadBufferUsed    int64 `json:"read_buffer_used"`
	ReadBufferMax     int64 `json:"read_buffer_max"`

	HasRemote      bool `json:"has_remote"`
	PendingSyncs   int  `json:"pending_syncs"`
	PendingUploads int  `json:"pending_uploads"`
	CompletedSyncs int  `json:"completed_syncs"`
	FailedSyncs    int  `json:"failed_syncs"`

	RemoteHealthy       bool    `json:"remote_healthy"`
	EvictionSuspended   bool    `json:"eviction_suspended"`
	OutageDurationSecs  float64 `json:"outage_duration_seconds"`
	OfflineReadsBlocked int64   `json:"offline_reads_blocked"`
}

// GetStats returns comprehensive block store statistics.
func (bs *BlockStore) GetStats() BlockStoreStats {
	localStats := bs.local.Stats()
	files := bs.local.ListFiles()

	rbStats := bs.readBuffer.Stats()

	pending, completed, failed := bs.syncer.Queue().Stats()
	_, uploads, _ := bs.syncer.Queue().PendingByType()

	remoteHealthy := bs.syncer.IsRemoteHealthy()
	outageDuration := bs.syncer.RemoteOutageDuration()

	stats := BlockStoreStats{
		FileCount:           len(files),
		LocalDiskUsed:       localStats.DiskUsed,
		LocalDiskMax:        localStats.MaxDisk,
		LocalMemUsed:        localStats.MemUsed,
		LocalMemMax:         localStats.MaxMemory,
		ReadBufferEntries:   rbStats.Entries,
		ReadBufferUsed:      rbStats.CurBytes,
		ReadBufferMax:       rbStats.MaxBytes,
		HasRemote:           bs.remote != nil,
		PendingSyncs:        pending,
		PendingUploads:      uploads,
		CompletedSyncs:      completed,
		FailedSyncs:         failed,
		RemoteHealthy:       remoteHealthy,
		EvictionSuspended:   bs.remote != nil && !remoteHealthy,
		OutageDurationSecs:  outageDuration.Seconds(),
		OfflineReadsBlocked: bs.syncer.OfflineReadsBlocked(),
	}

	bs.populateBlockCounts(&stats, files)

	return stats
}

// populateBlockCounts fills block count fields from the metadata store.
func (bs *BlockStore) populateBlockCounts(stats *BlockStoreStats, files []string) {
	if bs.fileBlockStore == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, payloadID := range files {
		blocks, err := bs.fileBlockStore.ListFileBlocks(ctx, payloadID)
		if err != nil {
			continue
		}
		for _, b := range blocks {
			stats.BlocksTotal++
			switch b.State {
			case blockstore.BlockStateDirty:
				stats.BlocksDirty++
			case blockstore.BlockStateLocal, blockstore.BlockStateSyncing:
				stats.BlocksLocal++
			case blockstore.BlockStateRemote:
				stats.BlocksRemote++
			}
		}
	}
}

// EvictReadBuffer clears all entries from the read buffer.
// Returns the number of entries that were cleared.
func (bs *BlockStore) EvictReadBuffer() int {
	entries := bs.readBuffer.Stats().Entries // nil-safe: returns zero
	bs.readBuffer.Close()                    // nil-safe: no-op
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
// When the read buffer is enabled, checks it first and fills it after successful read.
func (bs *BlockStore) readAtInternal(ctx context.Context, payloadID, cowSource string, data []byte, offset uint64) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	isPrimaryRead := cowSource == ""

	// Read buffer fast path: try to serve entirely from read buffer.
	// Only for primary reads (no COW source) with read buffer enabled.
	if bs.readBuffer != nil && isPrimaryRead {
		if n, ok := bs.tryL1Read(payloadID, data, offset); ok {
			bs.readBuffer.NotifyRead(payloadID, offset, uint64(len(data)), blockstore.BlockSize)
			return n, nil
		}
	}

	// Try primary local store.
	found, err := bs.local.ReadAt(ctx, payloadID, data, offset)
	if err != nil {
		return 0, fmt.Errorf("local read failed: %w", err)
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

	if err := bs.ensureAndReadFromLocal(ctx, payloadID, data, offset); err != nil {
		return 0, err
	}
	bs.promoteToL1(ctx, payloadID, offset, uint64(len(data)))

	return len(data), nil
}

// promoteToL1 fills the read buffer from the local store for the given byte
// range and notifies the prefetcher about the read. Both calls are nil-safe
// (no-op when the read buffer is disabled).
func (bs *BlockStore) promoteToL1(ctx context.Context, payloadID string, offset, length uint64) {
	bs.readBuffer.FillFromStore(ctx, payloadID, offset, length, blockstore.BlockSize, bs.local.GetBlockData)
	bs.readBuffer.NotifyRead(payloadID, offset, length, blockstore.BlockSize)
}

// tryL1Read attempts to serve a read entirely from the read buffer.
// Returns (bytesRead, true) if all blocks in the range were in the buffer.
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
		n, hit := bs.readBuffer.Get(payloadID, blockIdx, buf, blockOff)
		if !hit || uint64(n) != readLen {
			return 0, false
		}
	}

	return len(data), true
}

// readFromCOWSource reads from the COW source and copies data to the primary local store.
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

	// Copy to primary local store for future reads (non-fatal if fails)
	if err := bs.local.WriteAt(ctx, payloadID, dest, offset); err != nil {
		logger.Debug("COW local write failed (non-fatal)", "payloadID", payloadID, "error", err)
	}

	return nil
}

// ensureAndReadFromLocal downloads blocks from remote if needed and reads from local store.
func (bs *BlockStore) ensureAndReadFromLocal(ctx context.Context, payloadID string, dest []byte, offset uint64) error {
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
		logger.Debug("Sparse block: miss after download, returning zeros",
			"payloadID", payloadID)
	}

	return nil
}
