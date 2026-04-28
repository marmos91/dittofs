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

	// cache is the Phase 12 CAS-keyed cache (CACHE-01..05). Phase 11's
	// block-coord ReadBuffer + standalone Prefetcher were folded into a
	// single Cache type in Plan 12-09. Never nil — the constructor
	// substitutes nullCache{} for a disabled budget so engine code does
	// not need defensive nil-checks (Null Object pattern, WARN-8).
	cache CacheInterface

	readBufferBytes int64 // budget for the cache (0 = disabled / Null Object)
	prefetchWorkers int   // stored from config, used in Start()
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
		readBufferBytes: cfg.ReadBufferBytes,
		prefetchWorkers: cfg.PrefetchWorkers,
	}
	// Cache is created later in Start (loadFn closure captures bs and
	// NewCache spawns workers immediately). For now mount the Null Object
	// so engine code can call bs.cache.* without nil-checks even before
	// Start runs.
	bs.cache = nullCache{}
	// Phase 12 Plan 07: thread the coordinator into the syncer so the
	// post-Flush hook (persistFileBlocksAfterFlush) can invoke
	// PersistFileBlocks under the caller's metadata txn. Plan 09 wires
	// the actual trigger from uploadOne success.
	if cfg.Syncer != nil && cfg.Coordinator != nil {
		cfg.Syncer.SetCoordinator(cfg.Coordinator)
	}
	// Phase 13 BSCAS-05 (Plan 07): wire the BlockStore back-reference
	// onto the Syncer so the file-level dedup short-circuit can reach
	// BlockStore.cache for surgical invalidation of orphaned speculative
	// chunks. Reading through the back-reference (instead of caching a
	// CacheInterface field on the Syncer at construction time) lets test
	// code swap `bs.cache = rec` post-construction and still observe the
	// invalidation — mirrors the TestClose_ClosesCache pattern.
	if cfg.Syncer != nil {
		cfg.Syncer.bs = bs
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

	// Wire the Cache in Start so the loadByHash closure captures bs and
	// NewCache spawns workers immediately. Phase 12 Plan 09: a single
	// Cache type replaces Phase 11's ReadBuffer + Prefetcher pair.
	// readBufferBytes is read out of cfg via a stash because cfg lives
	// only inside New; we recover it from bs's own state. Engine
	// constructor stashes the budget on the BlockStore so Start can read
	// it; if the budget is 0 we keep the Null Object.
	if bs.readBufferBytes > 0 {
		realCache := NewCache(bs.readBufferBytes, bs.prefetchWorkers, bs.loadByHash)
		if realCache != nil {
			bs.cache = realCache
		}
	}

	return nil
}

// loadByHash is the LoadByHashFn the Cache's prefetch workers call to
// pull a block by ContentHash. Phase 12 Plan 09: replaces the legacy
// loadBlock(payloadID, blockIdx) with a CAS-keyed loader. Looks up the
// FileBlock by hash to recover its local path, then reads from local
// store. Remote fallback is intentionally NOT wired here — prefetch
// is best-effort and shouldn't block on a remote round-trip; if the
// block isn't local, the next on-path read will pull it via the syncer.
//
// Plan 12-10 (CACHE-06): when fb.LocalPath is set, use readFromCAS
// (build-tagged: mmap on linux/darwin, os.ReadFile on windows) for a
// single-copy load (page cache -> dest). Falls back to the legacy
// local.GetBlockData path when DataSize is unknown (legacy FileBlock
// rows without the post-Plan-10 DataSize attribute).
func (bs *BlockStore) loadByHash(ctx context.Context, hash blockstore.ContentHash) ([]byte, error) {
	if bs.fileBlockStore == nil {
		return nil, errors.New("loadByHash: fileBlockStore not wired")
	}
	fb, err := bs.fileBlockStore.GetByHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	if fb == nil || fb.LocalPath == "" {
		return nil, errors.New("loadByHash: block not local")
	}

	// CACHE-06 single-copy fast path: when DataSize is known, allocate
	// the destination buffer and read directly via the platform-aware
	// mmap/ReadFile primitive.
	if fb.DataSize > 0 {
		buf := make([]byte, fb.DataSize)
		n, err := readFromCAS(fb.LocalPath, 0, buf)
		if err == nil {
			return buf[:n], nil
		}
		// Fall through to the legacy path on any readFromCAS error
		// (e.g., the local file was rotated out from under us). The
		// legacy path consults the in-memory FileBlock state which
		// may still have the bytes in flight.
	}

	// Legacy fallback: read through the local store. Returns a heap
	// buffer the local store owns; we hand it directly to the caller.
	data, _, err := bs.local.GetBlockData(ctx, fb.ID, 0)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Close releases resources held by the store. Closes the cache (stops
// prefetch workers and drops entries), then syncer (drains uploads),
// local store, and remote store.
func (bs *BlockStore) Close() error {
	// Cache is never nil thanks to the Null Object pattern.
	_ = bs.cache.Close()

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
// 12 API-01: a non-nil/non-empty []BlockRef carries the CAS hashes
// covering the requested range (zero-filling sparse holes per D-21).
//
// Plan 12-09 wiring: after a successful read the engine calls
// cache.OnRead(payloadID, blockHashes, fileSize) so the Cache's
// sequential-detection state machine can fire prefetch on upcoming
// hashes. The actual byte-serving from cache.Get is a Plan 12-10
// (mmap) deliverable; for Plan 09 the cache is hint-only and reads
// always go through local/remote stores.
func (bs *BlockStore) ReadAt(ctx context.Context, payloadID string, blocks []blockstore.BlockRef, data []byte, offset uint64) (int, error) {
	n, err := bs.readAtInternal(ctx, payloadID, data, offset)
	if err != nil {
		return n, err
	}
	// Hint-only post-read: pass the BlockRef hashes and the maximal
	// file-size estimate so the Cache can decide on prefetch. nullCache
	// is a no-op so the unconditional call is safe (Null Object).
	if len(blocks) > 0 {
		hashes := blockRefHashes(blocks)
		bs.cache.OnRead(payloadID, hashes, computeFileSize(blocks))
	}
	return n, nil
}

// blockRefHashes extracts the ContentHash slice from a BlockRef list
// for OnRead's hint API.
func blockRefHashes(refs []blockstore.BlockRef) []blockstore.ContentHash {
	out := make([]blockstore.ContentHash, len(refs))
	for i, r := range refs {
		out[i] = r.Hash
	}
	return out
}

// computeFileSize returns the maximum (Offset + Size) across the
// BlockRef list — a conservative upper bound on file size used as
// the OnRead fileSize hint.
func computeFileSize(refs []blockstore.BlockRef) uint64 {
	var maxEnd uint64
	for _, r := range refs {
		end := r.Offset + uint64(r.Size)
		if end > maxEnd {
			maxEnd = end
		}
	}
	return maxEnd
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
	// Plan 12-09 D-35: cache invalidation moves OUT of the engine into
	// common.WriteToBlockStore (post-txn). The engine itself does NOT
	// touch cache on the write path beyond resetting the per-payload
	// sequential tracker via OnRead's empty-hashes signal — keeps
	// prefetch from chasing pre-write hashes after the underlying data
	// shifted. nullCache is a no-op (Null Object).
	bs.cache.OnRead(payloadID, nil, 0)
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
	// WR-03 (Phase 12 review iteration 1): coordinator decrements run FIRST
	// so a refcount-bookkeeping failure leaves the file untouched on disk
	// and remote. Previous order (local → cache → syncer → coordinator)
	// could leave 4-of-5 hashes leaked when step 4 failed mid-loop because
	// local data was already gone and remote had been swept. Mirrors the
	// engine.Delete ordering (D-17) and the documented Phase 12 stance
	// "orphan-not-deleted is preferred over live-data-deleted".
	//
	// CAS-path BlockRef pruning + coordinator DecrementRefCount per
	// dropped hash. Empty input (legacy/dual-read path) skips the
	// coordinator and returns nil so the caller's PutFile keeps
	// FileAttr.Blocks untouched.
	var kept []blockstore.BlockRef
	if len(currentBlocks) > 0 {
		kept = make([]blockstore.BlockRef, 0, len(currentBlocks))
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
	}

	if err := bs.local.Truncate(ctx, payloadID, newSize); err != nil {
		return currentBlocks, fmt.Errorf("local truncate failed: %w", err)
	}

	// Reset the per-payload sequential tracker (truncate invalidates
	// any in-flight prefetch state); cache entry invalidation is the
	// caller's responsibility via common.WriteToBlockStore (post-txn,
	// per D-35). nullCache is a no-op.
	bs.cache.OnRead(payloadID, nil, 0)

	// Remote sweep is best-effort: GC will reconcile stragglers, so a
	// failure here does NOT roll back the coordinator decrements (matches
	// engine.Delete semantics post-WR-04).
	if err := bs.syncer.Truncate(ctx, payloadID, newSize); err != nil {
		return kept, err
	}

	if len(currentBlocks) == 0 {
		return nil, nil
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
	// Surgical invalidation: drop ALL hashes belonging to this file
	// (even though dedup-shared hashes might survive elsewhere — Delete
	// is the strongest signal). nullCache is a no-op; for the real
	// Cache this also clears the per-payload sequential tracker.
	if len(blocks) > 0 {
		bs.cache.InvalidateFile(payloadID, blockRefHashes(blocks))
	} else {
		// Legacy/dual-read empty-blocks path: at least reset the
		// per-payload tracker so prefetch doesn't chase stale hashes.
		bs.cache.OnRead(payloadID, nil, 0)
	}

	// Phase 12 D-17: decrement RefCount for every BlockRef hash before
	// remote cleanup so the coordinator's bookkeeping is consistent
	// even if the remote sweep fails (Truncate / janitor will reconcile
	// orphans). Empty blocks (legacy / dual-read shim) skips the
	// coordinator entirely.
	//
	// WR-04 (Phase 12 review iteration 1): continue past coordinator
	// errors so the syncer.Delete remote sweep ALWAYS runs. Returning
	// early left the local data deleted, the metadata partially
	// decremented, and the remote alive forever — operators saw
	// inconsistent state until GC's next pass (hours). Now we capture
	// the first coordinator error, finish decrementing the rest, run
	// the remote sweep unconditionally, and return errors.Join of both
	// surfaces so the caller sees the full picture.
	var coordErr error
	if len(blocks) > 0 && bs.coordinator != nil {
		for _, b := range blocks {
			if _, err := bs.coordinator.DecrementRefCount(ctx, b.Hash); err != nil {
				if coordErr == nil {
					coordErr = fmt.Errorf("decrement refcount on delete %s: %w", b.Hash.String(), err)
				}
			}
		}
	}

	if delErr := bs.syncer.Delete(ctx, payloadID); delErr != nil {
		if coordErr != nil {
			return errors.Join(coordErr, delErr)
		}
		return delErr
	}
	return coordErr
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
//
// Phase 11 auto-promoted flushed blocks into the block-coord ReadBuffer
// to make subsequent reads cheap (data was in OS page cache anyway).
// Plan 12-09 retires that path: the new Cache is CAS-keyed and Flush
// has no BlockRef snapshot at this layer to translate flushed bytes
// into hash-keyed cache entries. Auto-promotion will be revisited in
// Plan 12-10 (mmap variant) which sidesteps the heap-copy-on-Put cost
// that motivated the auto-promote in the first place.
func (bs *BlockStore) Flush(ctx context.Context, payloadID string) (*blockstore.FlushResult, error) {
	return bs.syncer.Flush(ctx, payloadID)
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

	cacheStats := bs.cache.Stats()

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
		ReadBufferEntries:   cacheStats.Entries,
		ReadBufferUsed:      cacheStats.CurBytes,
		ReadBufferMax:       cacheStats.MaxBytes,
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

// EvictReadBuffer clears all entries from the cache (legacy method
// name retained for the controlplane runtime's blockStores.evict REST
// path; behavior post-Plan-12-09 closes the cache, which Plan-12-10
// will rework once mmap-backed entries change the eviction story).
// Returns the number of entries that were present before close.
func (bs *BlockStore) EvictReadBuffer() int {
	entries := bs.cache.Stats().Entries
	_ = bs.cache.Close()
	// Replace closed cache with the Null Object so subsequent operations
	// remain a no-op without ever returning an error path.
	bs.cache = nullCache{}
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

// readAtInternal reads from the primary payloadID. Always goes through
// the local store (with remote-fallback on miss); the Plan 12-09 Cache
// is hint-only and does not serve bytes here. The CAS-keyed byte-serve
// path is a Plan 12-10 (mmap) deliverable.
func (bs *BlockStore) readAtInternal(ctx context.Context, payloadID string, data []byte, offset uint64) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	// Try primary local store.
	found, err := bs.local.ReadAt(ctx, payloadID, data, offset)
	if err != nil {
		return 0, fmt.Errorf("local read failed: %w", err)
	}
	if found {
		return len(data), nil
	}

	if err := bs.ensureAndReadFromLocal(ctx, payloadID, data, offset); err != nil {
		return 0, err
	}

	return len(data), nil
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
