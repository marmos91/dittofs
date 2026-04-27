package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
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
	Syncer *Syncer

	// FileBlockStore provides block metadata for block store statistics
	// AND the engine-internal lookups (GetFileBlock, ListFileBlocks) the
	// dual-read resolver and populateBlockCounts still consume — see
	// blockstore.EngineFileBlockStore. When set, GetStats() populates
	// BlocksLocal/BlocksRemote/BlocksTotal.
	FileBlockStore blockstore.EngineFileBlockStore

	// Coordinator handles all metadata-store operations the engine
	// needs (RefCount mutations, BlockRef-list persistence). Phase 12
	// API-02: keeps pkg/metadata out of the engine hot path. May be
	// nil in tests; production wiring (pkg/controlplane/runtime/shares/
	// service.go) MUST inject a real impl. See coordinator.go for the
	// contract.
	Coordinator MetadataCoordinator

	// ReadBufferBytes is the memory budget for the read buffer per share.
	// 0 disables the read buffer. Passed directly to NewReadBuffer as byte budget.
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
	syncer *Syncer

	// Phase 12 (META-03 / D-09): widened to EngineFileBlockStore so
	// populateBlockCounts can call ListFileBlocks (engine-internal method
	// not on the public FileBlockStore surface).
	fileBlockStore blockstore.EngineFileBlockStore // optional: for block count stats

	// coordinator handles all metadata-store operations the engine
	// needs (RefCount mutations, BlockRef-list persistence). May be nil
	// in tests; production wiring (pkg/controlplane/runtime/shares/
	// service.go) MUST inject a real impl. See coordinator.go for the
	// contract.
	coordinator MetadataCoordinator

	readBuffer *ReadBuffer // nil when disabled (ReadBufferBytes=0)
	prefetcher *Prefetcher // nil when disabled (PrefetchWorkers=0 or readBuffer nil)

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

	bs := &BlockStore{
		local:           cfg.Local,
		remote:          cfg.Remote,
		syncer:          cfg.Syncer,
		fileBlockStore:  cfg.FileBlockStore,
		coordinator:     cfg.Coordinator,
		readBuffer:      NewReadBuffer(cfg.ReadBufferBytes),
		prefetchWorkers: cfg.PrefetchWorkers,
	}
	// Phase 12 Plan 07: thread the coordinator into the syncer so the
	// post-Flush hook (persistFileBlocksAfterFlush) can invoke
	// PersistFileBlocks under the caller's metadata txn. Plan 09 wires
	// the actual trigger from uploadOne success.
	if cfg.Syncer != nil && cfg.Coordinator != nil {
		cfg.Syncer.SetCoordinator(cfg.Coordinator)
	}
	return bs, nil
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
		bs.prefetcher = NewPrefetcher(
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

// ReadAt reads data from storage at the given offset into dest. Phase
// 12 API-01: a non-nil/non-empty []BlockRef triggers the CAS read path
// (findBlocksForRange-driven hash resolution), zero-filling sparse
// holes per D-21. Empty blocks falls through to the legacy Phase 11
// dual-read shim (D-20).
//
// The CAS path itself is a Plan 09 deliverable (cache rewrite). Plan 07
// wires the signature so callers can pass []BlockRef without breaking
// the legacy path; the actual CAS-routing body lands in Plan 09. Until
// then, every ReadAt call routes through the dual-read shim regardless
// of blocks contents — this is documented in Plan 07 SUMMARY.
func (bs *BlockStore) ReadAt(ctx context.Context, payloadID string, blocks []blockstore.BlockRef, data []byte, offset uint64) (int, error) {
	_ = blocks // CAS routing wired in Plan 09; Plan 07 lays the API only.
	return bs.readAtInternal(ctx, payloadID, data, offset)
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

// WriteAt writes data to storage at the given offset and returns the
// new BlockRef list. Writes go directly to the local store; the syncer
// handles background upload. Read buffer entries for affected blocks
// are invalidated and prefetcher is reset.
//
// Phase 12 API-01: signature returns []BlockRef so the caller can
// persist FileAttr.Blocks in the same metadata txn. Plan 07 wires the
// API; the FastCDC re-chunking and merged-BlockRef construction land
// in Plan 09 alongside the cache rewrite. Until then, this method
// returns currentBlocks unchanged — the legacy syncer path still
// drives the actual upload; FileAttr.Blocks is populated lazily by
// the dual-read shim. This is the documented Plan 07 contract.
func (bs *BlockStore) WriteAt(ctx context.Context, payloadID string, currentBlocks []blockstore.BlockRef, data []byte, offset uint64) ([]blockstore.BlockRef, error) {
	if len(data) == 0 {
		return currentBlocks, nil
	}
	if err := bs.local.WriteAt(ctx, payloadID, data, offset); err != nil {
		return currentBlocks, err
	}
	bs.readBuffer.InvalidateRange(payloadID, offset, len(data), blockstore.BlockSize)
	// Plan 09 will return the merged []BlockRef list here; for Plan 07
	// the legacy path's FileAttr.Blocks is unchanged (dual-read shim).
	return currentBlocks, nil
}

// Truncate changes the size of a payload in both local store and remote
// store. Invalidates read buffer entries above the new size and resets
// prefetcher state.
//
// Phase 12 API-01/D-15: when currentBlocks is non-empty, blocks
// strictly past newSize are dropped and the coordinator decrements
// RefCount for each dropped hash. The new []BlockRef list is returned
// for the caller to persist via PutFile. When currentBlocks is empty
// the legacy path runs and the returned slice is empty (dual-read
// shim semantics).
func (bs *BlockStore) Truncate(ctx context.Context, payloadID string, currentBlocks []blockstore.BlockRef, newSize uint64) ([]blockstore.BlockRef, error) {
	if err := bs.local.Truncate(ctx, payloadID, newSize); err != nil {
		return currentBlocks, fmt.Errorf("local truncate failed: %w", err)
	}

	bs.readBuffer.InvalidateAboveSize(payloadID, newSize, blockstore.BlockSize)

	if err := bs.syncer.Truncate(ctx, payloadID, newSize); err != nil {
		return currentBlocks, err
	}

	// CAS-path BlockRef pruning + coordinator DecrementRefCount per
	// dropped hash. Empty input (legacy/dual-read path) returns nil so
	// the caller's PutFile keeps FileAttr.Blocks untouched.
	if len(currentBlocks) == 0 {
		return nil, nil
	}
	kept := make([]blockstore.BlockRef, 0, len(currentBlocks))
	for _, b := range currentBlocks {
		if b.Offset >= newSize {
			// Block fully past newSize — drop it.
			if bs.coordinator != nil {
				if _, err := bs.coordinator.DecrementRefCount(ctx, b.Hash); err != nil {
					return currentBlocks, fmt.Errorf("decrement refcount on truncate-drop %s: %w", b.Hash.String(), err)
				}
			}
			continue
		}
		// Block fully or partially before newSize — keep. Plan 09 will
		// re-chunk the partial-tail block; Plan 07 keeps it as-is.
		kept = append(kept, b)
	}
	return kept, nil
}

// Delete removes all data for a payload from local store and remote store.
// Invalidates all read buffer entries for the file and resets prefetcher state.
//
// Local cleanup uses DeleteAllBlockFiles (not EvictMemory) so on-disk .blk
// files are removed alongside in-memory state. TD-02c: previously only memory
// was evicted, which left orphan .blk files growing unbounded across
// delete-and-recreate workloads.
//
// SyncFileBlocksForFile runs first so any FileBlock metadata that is still
// queued (queueFileBlockUpdate after flushBlock) is persisted before
// DeleteAllBlockFiles enumerates the store — otherwise freshly-flushed .blk
// files would be missed and leaked.
func (bs *BlockStore) Delete(ctx context.Context, payloadID string, blocks []blockstore.BlockRef) error {
	bs.local.SyncFileBlocksForFile(ctx, payloadID)
	if err := bs.local.DeleteAllBlockFiles(ctx, payloadID); err != nil {
		return fmt.Errorf("local delete all block files failed: %w", err)
	}
	bs.readBuffer.InvalidateAndReset(payloadID)

	// Phase 12 D-17: decrement RefCount for every BlockRef hash before
	// remote cleanup so the coordinator's bookkeeping is consistent
	// even if the remote sweep fails (Truncate / janitor will reconcile
	// orphans). Empty blocks (legacy / dual-read shim) skips the
	// coordinator entirely.
	if len(blocks) > 0 && bs.coordinator != nil {
		for _, b := range blocks {
			if _, err := bs.coordinator.DecrementRefCount(ctx, b.Hash); err != nil {
				return fmt.Errorf("decrement refcount on delete %s: %w", b.Hash.String(), err)
			}
		}
	}

	return bs.syncer.Delete(ctx, payloadID)
}

// CopyPayload duplicates a file's BlockRef list with O(1) cost (Phase
// 12 D-11). Increments the RefCount of each unique source-hash via the
// coordinator (no per-block data copy); returns a deep copy of
// srcBlocks as the destination's BlockRef list. The caller's metadata
// txn rolls back all increments on any error.
//
// Empty srcBlocks => nil-safe legacy path: copies nothing (legacy
// CopyPayload data-copy semantics are removed in Plan 07; the legacy
// adapter call sites that need data copies should drive ReadAt+WriteAt
// directly during the dual-read window). Production callers always
// supply a snapshot of the source file's FileAttr.Blocks.
//
// Failure semantics: on any IncrementRefCount error, returns the error
// immediately without further increments. Already-bumped counts are
// the caller's metadata txn's responsibility to roll back (the engine
// owns no txn — D-11 / BLOCKER-1/2/3 resolution).
//
// Dedup: a single hash present multiple times in srcBlocks bumps the
// RefCount only once per CopyPayload call (per-call seen-hash set).
// The destination's []BlockRef preserves the original sequence so
// subsequent reads still resolve every offset correctly.
func (bs *BlockStore) CopyPayload(ctx context.Context, srcPayloadID, dstPayloadID string, srcBlocks []blockstore.BlockRef) ([]blockstore.BlockRef, error) {
	// Empty src => no work, nothing to coordinate.
	if len(srcBlocks) == 0 {
		return nil, nil
	}
	if bs.coordinator == nil {
		return nil, ErrMetadataCoordinatorNotWired
	}

	// Increment RefCount once per unique hash. Track seen so duplicate
	// hashes (a single CAS object referenced by multiple BlockRefs in
	// the same file — Phase 13 file-level dedup) are bumped exactly
	// once per CopyPayload call.
	seen := make(map[blockstore.ContentHash]struct{}, len(srcBlocks))
	for _, b := range srcBlocks {
		if _, ok := seen[b.Hash]; ok {
			continue
		}
		seen[b.Hash] = struct{}{}
		if err := bs.coordinator.IncrementRefCount(ctx, b.Hash); err != nil {
			return nil, fmt.Errorf("CopyPayload: increment refcount on %s: %w", b.Hash.String(), err)
		}
	}

	// Deep-copy the slice (BlockRef is a value type — append over nil
	// produces a fresh backing array independent of srcBlocks).
	dst := append([]blockstore.BlockRef(nil), srcBlocks...)

	// Note: src/dst payloadIDs are kept in the signature for future use
	// (cache prefetch hints, identity-based dedup) and to match the
	// public Writer interface; the O(1) implementation does not need
	// them for the refcount-only fast path.
	_ = srcPayloadID
	_ = dstPayloadID

	return dst, nil
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
		return health.NewUnknownReport(err.Error(), time.Since(start))
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
			case blockstore.BlockStatePending:
				// Pending now covers both legacy Dirty (no LocalPath / no key)
				// and Local (complete, awaiting sync). Distinguish by data
				// state to keep the existing introspection counters meaningful.
				if b.LocalPath != "" || b.BlockStoreKey != "" {
					stats.BlocksLocal++
				} else {
					stats.BlocksDirty++
				}
			case blockstore.BlockStateSyncing:
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

// readAtInternal reads from the primary payloadID.
// When the read buffer is enabled, checks it first and fills it after successful read.
func (bs *BlockStore) readAtInternal(ctx context.Context, payloadID string, data []byte, offset uint64) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	// Read buffer fast path: try to serve entirely from read buffer.
	if bs.readBuffer != nil {
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
		bs.promoteToL1(ctx, payloadID, offset, uint64(len(data)))
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
