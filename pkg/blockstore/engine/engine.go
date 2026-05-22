package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/chunker"
	"github.com/marmos91/dittofs/pkg/blockstore/local"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	"github.com/marmos91/dittofs/pkg/health"
	"github.com/marmos91/dittofs/pkg/metadata"
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
	// needs (RefCount mutations, BlockRef-list persistence). API-02:
	// keeps pkg/metadata out of the engine hot path. May be nil in
	// tests; production wiring (pkg/controlplane/runtime/shares/
	// service.go) MUST inject a real impl. See coordinator.go for the
	// contract.
	Coordinator MetadataCoordinator

	// SyncedHashStore persists per-CAS-hash local→remote mirror state.
	// Sourced from the same per-share metadata-store handle the
	// Coordinator wraps. Threaded through to the Syncer so the mirror
	// loop in Flush can call MarkSynced after each successful
	// remote.Put. Nil is accepted (local-only / no-remote fixtures);
	// the Syncer's mirror loop early-exits in that mode.
	SyncedHashStore metadata.SyncedHashStore

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

	// META-03: widened to EngineFileBlockStore so populateBlockCounts
	// can call ListFileBlocks (engine-internal method not on the public
	// FileBlockStore surface).
	fileBlockStore blockstore.EngineFileBlockStore // optional: for block count stats

	// coordinator handles all metadata-store operations the engine
	// needs (RefCount mutations, BlockRef-list persistence). May be nil
	// in tests; production wiring (pkg/controlplane/runtime/shares/
	// service.go) MUST inject a real impl. See coordinator.go for the
	// contract.
	coordinator MetadataCoordinator

	// syncedHashStore persists per-CAS-hash local→remote mirror state.
	// Held alongside the coordinator so the engine constructor can
	// thread it into both the Syncer (via SetSyncedHashStore) and the
	// FSStore (via the ObjectIDPersister callback target's sibling
	// SetSyncedHashStore on the local store). May be nil in tests.
	syncedHashStore metadata.SyncedHashStore

	// cache is the CAS-keyed cache (CACHE-01..05). The block-coord
	// ReadBuffer + standalone Prefetcher are folded into this single
	// Cache type. Never nil — the constructor substitutes nullCache{}
	// for a disabled budget so engine code does not need defensive
	// nil-checks (Null Object pattern).
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
		syncedHashStore: cfg.SyncedHashStore,
		readBufferBytes: cfg.ReadBufferBytes,
		prefetchWorkers: cfg.PrefetchWorkers,
	}
	// Cache is created later in Start (loadFn closure captures bs and
	// NewCache spawns workers immediately). For now mount the Null Object
	// so engine code can call bs.cache.* without nil-checks even before
	// Start runs.
	bs.cache = nullCache{}
	// Thread the coordinator into the syncer so the file-level dedup
	// short-circuit (engine.Flush's pre-rollup hook) can call into
	// trySpeculativeFileLevelDedup with a real coordinator. cfg.Syncer
	// is guaranteed non-nil by the required-field check above.
	if cfg.Coordinator != nil {
		cfg.Syncer.SetCoordinator(cfg.Coordinator)
	}
	// Thread the SyncedHashStore into the Syncer so the mirror loop in
	// Flush can call MarkSynced after each remote.Put. Nil is accepted
	// (local-only / no-remote fixtures); the mirror loop early-exits in
	// that mode.
	if cfg.SyncedHashStore != nil {
		cfg.Syncer.SetSyncedHashStore(cfg.SyncedHashStore)
	}
	// Install the rollup-completion ObjectIDPersister callback on the
	// local store if it supports the setter. The callback (1) writes
	// per-block FileBlock rows so the engine's CAS read path
	// (readLocalByHash → resolveFileBlock) can resolve
	// (payloadID, blockIdx) → hash and (2) delegates to the
	// coordinator's PersistFileBlocks so FileAttr.Blocks and
	// FileAttr.ObjectID land in a single metadata txn at rollup time.
	// Local stores that don't implement the setter (in-memory /
	// fixtures use the parallel ChunkEmitter hook below) silently skip
	// the install — ObjectID compute still runs inside rollup but the
	// persist step is no-op.
	if setter, ok := cfg.Local.(interface {
		SetObjectIDPersister(p func(ctx context.Context, payloadID string, blocks []blockstore.BlockRef, objectID blockstore.ObjectID) error)
	}); ok {
		fbs := cfg.FileBlockStore
		setter.SetObjectIDPersister(func(ctx context.Context, payloadID string, blocks []blockstore.BlockRef, objectID blockstore.ObjectID) error {
			// (1) Per-chunk FileBlock rows. The row ID encodes the
			//     chunk's absolute byte Offset directly (rather than
			//     a synthetic blockIdx = Offset / BlockSize); the
			//     engine's CAS read path uses the parsed Offset to
			//     locate which chunk covers a given byte range under
			//     FastCDC's variable chunk geometry. The trailing
			//     numeric component is the chunk Offset in bytes.
			if fbs != nil {
				for _, b := range blocks {
					if b.Hash.IsZero() {
						continue
					}
					fb := &blockstore.FileBlock{
						ID:       fmt.Sprintf("%s/%d", payloadID, b.Offset),
						Hash:     b.Hash,
						DataSize: b.Size,
						State:    blockstore.BlockStatePending,
					}
					// Crash-consistency (#583): surface the put error so
					// a failed FileBlock row write fails the rollup pass
					// rather than silently losing the manifest entry.
					// Without this surfacing the engine continues into
					// PersistFileBlocks below, the rollup_offset advances
					// past records whose manifest row never landed, and
					// subsequent reads hit the sparse-block zero-fill
					// branch — silent data loss.
					if err := fbs.Put(ctx, fb); err != nil {
						return fmt.Errorf("ObjectIDPersister: FileBlock.Put(%s): %w", fb.ID, err)
					}
				}
			}
			// (2) Manifest + ObjectID coordinator txn.
			if bs.coordinator == nil {
				return nil
			}
			return bs.coordinator.PersistFileBlocks(ctx, payloadID, blocks, objectID)
		})
	}
	// Phase 19 Opt 3 (D-10/D-11/D-16): install the chunk-completion
	// callback on local stores that expose the setter (production
	// *fs.FSStore does; the in-memory backend does not — its writes go
	// through SetChunkEmitter below and don't materialize through the
	// CAS chunkstore.StoreChunk + lruTouch path Plan 07 hooks). The
	// closure delegates every successful chunkstore write to
	// bs.cache.Put: the engine Cache becomes warm on the write side, so
	// the NFS COMMIT-then-READ pattern never goes back to disk for the
	// just-written chunk. The closure captures bs (not bs.cache) so the
	// Null-Object→real-Cache swap performed by BlockStore.Start at
	// engine.go:267-270 is observed transparently. The path arg is
	// intentionally discarded (`_ string`) — Cache.Put doesn't consume it;
	// the firing-site contract still passes it to enable future mmap-or-
	// copy strategies (cache.go docstring). Cache.Put is nil-safe + closed-
	// safe + max-bytes-safe (cache.go:229-235), so this binding is the
	// canonical safe wiring (RAM ceiling bounded by Cache's existing LRU,
	// D-11). Same lifecycle precedent as SetObjectIDPersister above —
	// install once at construction; FSStore guarantees no chunk activity
	// fires before Start completes.
	if setter, ok := cfg.Local.(interface {
		SetOnChunkComplete(fn func(hash blockstore.ContentHash, data []byte, path string))
	}); ok {
		setter.SetOnChunkComplete(func(hash blockstore.ContentHash, data []byte, _ string) {
			bs.cache.Put(hash, data)
		})
	}
	// Install a per-chunk emitter on local stores that expose one (the
	// in-memory backend uses this; *fs.FSStore drives the equivalent
	// rollup-side path through the ObjectIDPersister callback above).
	// The emitter mirrors each freshly-emitted CAS chunk into a
	// FileBlock row keyed by {payloadID}/{blockIdx} so the engine's
	// CAS read path (readLocalByHash) can resolve (payloadID, offset)
	// → hash without a separate manifest. blockIdx is derived from
	// chunkStart / blockstore.BlockSize — works correctly for the
	// in-memory backend's small / aligned test workloads. Production
	// FSStore writes its own FileBlock rows through the
	// rollup.PersistFileBlocks path and never installs the emitter.
	if emitter, ok := cfg.Local.(interface {
		SetChunkEmitter(emit func(payloadID string, chunkStart uint64, size uint32, hash blockstore.ContentHash))
	}); ok && cfg.FileBlockStore != nil {
		fbs := cfg.FileBlockStore
		emitter.SetChunkEmitter(func(payloadID string, chunkStart uint64, size uint32, hash blockstore.ContentHash) {
			fb := &blockstore.FileBlock{
				ID:       fmt.Sprintf("%s/%d", payloadID, chunkStart),
				Hash:     hash,
				DataSize: size,
				State:    blockstore.BlockStatePending,
			}
			// Crash-consistency (#583): emitter signature is void by
			// contract; a put failure here means the manifest row never
			// landed and reads will sparse-zero that range. Log at Error
			// so operators see the loss instead of silently swallowing
			// it. Follow-up: promote emitter to return an error so the
			// caller can fail the rollup pass.
			if err := fbs.Put(context.Background(), fb); err != nil {
				logger.Error("ChunkEmitter: FileBlock.Put failed", "id", fb.ID, "error", err)
			}
		})
	}
	// BSCAS-05: wire the BlockStore back-reference onto the Syncer so
	// the file-level dedup short-circuit can reach BlockStore.cache for
	// surgical invalidation of orphaned speculative chunks. Reading
	// through the back-reference (instead of caching a CacheInterface
	// field on the Syncer at construction time) lets test code swap
	// `bs.cache = rec` post-construction and still observe the
	// invalidation — mirrors the TestClose_ClosesCache pattern.
	cfg.Syncer.bs = bs
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
	// NewCache spawns workers immediately. A single Cache type replaces
	// the legacy ReadBuffer + Prefetcher pair. readBufferBytes is read
	// out of cfg via a stash because cfg lives only inside New; we
	// recover it from bs's own state. Engine constructor stashes the
	// budget on the BlockStore so Start can read it; if the budget is
	// 0 we keep the Null Object.
	if bs.readBufferBytes > 0 {
		realCache := NewCache(bs.readBufferBytes, bs.prefetchWorkers, bs.loadByHash)
		if realCache != nil {
			bs.cache = realCache
		}
	}

	return nil
}

// loadByHash is the LoadByHashFn the Cache's prefetch workers call to
// pull a block by ContentHash. It performs a single content-addressed
// local read; local.Get is the only primitive (no mmap fast-path, no
// legacy FileBlock → GetBlockData fallback).
//
// Buffer ownership: local.Get returns a freshly allocated []byte; the
// Cache copies those bytes into its LRU slot on miss. The net
// allocation count matches the legacy mmap-then-copy semantics — the
// alloc just moves earlier in the pipeline.
//
// Remote fallback is intentionally NOT wired here — prefetch is
// best-effort and shouldn't block on a remote round-trip; if the
// block isn't local, the next on-path read will pull it via the
// syncer.
func (bs *BlockStore) loadByHash(ctx context.Context, hash blockstore.ContentHash) ([]byte, error) {
	return bs.local.Get(ctx, hash)
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

// ReadAt reads data from storage at the given offset into dest. API-01:
// a non-nil/non-empty []BlockRef carries the CAS hashes covering the
// requested range (zero-filling sparse holes).
//
// After a successful read the engine calls cache.OnRead(payloadID,
// blockHashes, fileSize) so the Cache's sequential-detection state
// machine can fire prefetch on upcoming hashes. The cache is hint-only
// here; reads always go through local/remote stores.
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
// API-01: signature returns []BlockRef so the caller can persist
// FileAttr.Blocks in the same metadata txn.
//
// WriteAt remains a per-write append into the local store — it does
// NOT chunk or assemble BlockRefs. The FastCDC chunker runs at the
// local-store rollup layer (pkg/blockstore/local/fs/rollup.go::
// rollupFile), which produces Pending FileBlocks carrying chunk
// hashes. Syncer.Flush projects ListFileBlocks(payloadID) into the
// canonical sorted []BlockRef list at quiesce time and invokes either
// the file-level dedup short-circuit (BSCAS-05) or the per-block
// upload pump + post-Flush hook (BSCAS-04 / META-02). FileAttr.Blocks
// AND FileAttr.ObjectID are written in the same metadata transaction
// by the runtime coordinator's PersistFileBlocks.
//
// Returns currentBlocks unchanged — the canonical projection happens
// at Flush time, not WriteAt time.
func (bs *BlockStore) WriteAt(ctx context.Context, payloadID string, currentBlocks []blockstore.BlockRef, data []byte, offset uint64) ([]blockstore.BlockRef, error) {
	if len(data) == 0 {
		return currentBlocks, nil
	}
	if err := bs.local.AppendWrite(ctx, payloadID, data, offset); err != nil {
		return currentBlocks, err
	}
	// Cache invalidation lives in common.WriteToBlockStore (post-txn),
	// not here. The engine itself does NOT touch cache on the write
	// path beyond resetting the per-payload sequential tracker via
	// OnRead's empty-hashes signal — keeps prefetch from chasing
	// pre-write hashes after the underlying data shifted. nullCache is
	// a no-op (Null Object).
	bs.cache.OnRead(payloadID, nil, 0)
	// BSCAS-05: the FastCDC chunker output is
	// produced by the local-store rollup pump
	// (pkg/blockstore/local/fs/rollup.go::rollupFile) and lands as
	// Pending FileBlocks with chunk-hash populated. The canonical
	// []BlockRef projection is built at Flush time from
	// ListFileBlocks(payloadID) — see Syncer.snapshotPendingBlockRefs
	// (file-level dedup short-circuit input) and Syncer.snapshotBlockRefs
	// (post-drain canonical list for the post-Flush hook). WriteAt
	// itself remains a per-write append into the local store and does
	// NOT need to return a merged []BlockRef; the dual-read shim's
	// currentBlocks pass-through is preserved for callers that have not
	// yet migrated to FileAttr.Blocks reads.
	return currentBlocks, nil
}

// Truncate changes the size of a payload in both local store and remote
// store. Invalidates read buffer entries above the new size and resets
// prefetcher state.
//
// API-01: when currentBlocks is non-empty, blocks strictly past newSize
// are dropped and the coordinator decrements RefCount for each dropped
// hash. The new []BlockRef list is returned for the caller to persist
// via PutFile. When currentBlocks is empty the legacy path runs and
// the returned slice is empty (dual-read shim semantics).
func (bs *BlockStore) Truncate(ctx context.Context, payloadID string, currentBlocks []blockstore.BlockRef, newSize uint64) ([]blockstore.BlockRef, error) {
	// WR-03: coordinator decrements run FIRST so a refcount-bookkeeping
	// failure leaves the file untouched on disk and remote. Previous
	// order (local → cache → syncer → coordinator) could leave 4-of-5
	// hashes leaked when step 4 failed mid-loop because local data was
	// already gone and remote had been swept. Mirrors the engine.Delete
	// ordering — "orphan-not-deleted is preferred over
	// live-data-deleted".
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
	// caller's responsibility via common.WriteToBlockStore (post-txn).
	// nullCache is a no-op.
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
// Local cleanup runs in this order under the unified CAS surface:
//  1. SyncFileBlocksForFile persists any in-flight FileBlock metadata so
//     the refcount decrements below operate on the authoritative manifest
//     for the file (see "blocks" arg).
//  2. EvictMemory drops the per-file in-memory tracking (memBlocks, files
//     map, accessTracker entry). There are no legacy per-file block files
//     to remove — the CAS chunk store under blocks/<hh>/ is the only
//     on-disk layout, and individual chunks are reclaimed via refcount →
//     GC, not per-file enumeration.
//  3. DeleteLog tombstones and removes the per-file append log so any
//     pre-rollup bytes are discarded.
//
// Subsequent steps (cache invalidate, coordinator refcount decrements,
// optional remote sweep) are unchanged.
func (bs *BlockStore) Delete(ctx context.Context, payloadID string, blocks []blockstore.BlockRef) error {
	bs.local.SyncFileBlocksForFile(ctx, payloadID)
	if err := bs.local.EvictMemory(ctx, payloadID); err != nil {
		return fmt.Errorf("local evict memory failed: %w", err)
	}
	if err := bs.local.DeleteLog(ctx, payloadID); err != nil {
		return fmt.Errorf("local delete append log failed: %w", err)
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

	// Decrement RefCount for every BlockRef hash before remote cleanup
	// so the coordinator's bookkeeping is consistent even if the remote
	// sweep fails (Truncate / janitor will reconcile orphans). Empty
	// blocks (legacy / dual-read shim) skips the coordinator entirely.
	//
	// WR-04: continue past coordinator errors so the syncer.Delete
	// remote sweep ALWAYS runs. Returning early left the local data
	// deleted, the metadata partially decremented, and the remote alive
	// forever — operators saw inconsistent state until GC's next pass
	// (hours). Now we capture the first coordinator error, finish
	// decrementing the rest, run the remote sweep unconditionally, and
	// return errors.Join of both surfaces so the caller sees the full
	// picture.
	var coordErr error
	if len(blocks) > 0 && bs.coordinator != nil {
		for _, b := range blocks {
			newCount, err := bs.coordinator.DecrementRefCount(ctx, b.Hash)
			if err != nil {
				if coordErr == nil {
					coordErr = fmt.Errorf("decrement refcount on delete %s: %w", b.Hash.String(), err)
				}
				continue
			}
			// Refcount hit zero: the local CAS chunk is being reclaimed,
			// so drop the synced marker too. Without this cascade the
			// synced set would drift out of strict-subset relationship
			// with local CAS contents — a future re-Put of the same hash
			// would skip remote upload because the marker is stale.
			// Failure here is benign (the marker becomes an orphan, but
			// a stale marker only causes a single skipped upload on a
			// re-Put; the bytes are already remote-resident from the
			// original Mark). Logged at Warn for operator visibility.
			if newCount == 0 && bs.syncedHashStore != nil {
				if derr := bs.syncedHashStore.DeleteSynced(ctx, b.Hash); derr != nil {
					logger.Warn("delete synced marker (orphan; benign)",
						"hash", b.Hash.String(), "err", derr)
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

// CopyPayload duplicates a file's BlockRef list with O(1) cost.
// Increments the RefCount of each unique source-hash via the
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
// owns no txn).
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
	// the same file — file-level dedup) are bumped exactly
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
// Pre-rollup file-level dedup hook: when a coordinator is wired and the
// file's speculative BlockRef manifest is non-empty, the engine asks the
// coordinator whether a previously-quiesced file with the same Merkle
// root already exists. On hit the upload pump is bypassed entirely —
// FileAttr.Blocks is swapped to the target's BlockRef list, refcounts
// are reconciled, and Flush returns Finalized=true without delegating
// to the syncer. On miss / nil-coordinator the syncer's mirror loop
// runs as usual.
//
// Auto-promote into the read buffer is intentionally NOT done here:
// the Cache is CAS-keyed and Flush has no BlockRef snapshot at this
// layer to translate flushed bytes into hash-keyed cache entries.
func (bs *BlockStore) Flush(ctx context.Context, payloadID string) (*blockstore.FlushResult, error) {
	// Both pre-rollup dedup hooks require a coordinator; gate them
	// jointly so the nil-check isn't repeated.
	if bs.coordinator != nil {
		// Phase 19 Opt 4 (D-13/D-14/D-16): eager small-file dedup BEFORE
		// the speculative path. Files at or below chunker.MinChunkSize
		// (1 MiB) emit a single chunk under FastCDC anyway; hashing the
		// whole content in RAM and consulting metadata.FindByObjectID
		// skips chunker + log + CAS write entirely on hit. Sibling fast-
		// path; shares applyFileLevelDedupHit's finalize machinery so
		// STATE-01..03 + cache invalidation + D-11 appendlog cleanup
		// invariants remain identical to the speculative path.
		//
		// Source-of-truth for the in-RAM bytes: bs.local.ReadPayloadAt
		// consults the per-payload appendlog (pre-rollup bytes) before
		// the FileBlock manifest, which is the right surface — eager runs
		// BEFORE the rollup commits anything to CAS. For local stores that
		// have already rolled up (the in-memory backend's synchronous
		// rollup, FSStore steady state), ReadPayloadAt walks the manifest
		// and serves the same bytes from the now-stored chunks; the eager
		// path's hash + lookup are identical either way.
		//
		// Outer size gate at the call site is intentionally defensive
		// (tryEagerSmallFileDedup re-checks internally — that gate is the
		// real authority) but lets us skip the ReadPayloadAt alloc + I/O
		// entirely for large files.
		if size, found := bs.local.GetFileSize(ctx, payloadID); found && size > 0 && size <= chunker.MinChunkSize {
			// Outer gate already bounds size to chunker.MinChunkSize (1 MiB),
			// well below math.MaxInt on every supported platform. The cast
			// here is therefore safe; the explicit form documents the
			// bounded-uint64->int conversion for readers and linters.
			isize := int(size)
			data := make([]byte, isize)
			n, err := bs.local.ReadPayloadAt(ctx, payloadID, data, 0)
			// On a clean read we have the full payload in RAM; consult
			// eager dedup. A short / errored read is treated as "skip
			// eager and fall through to speculative" — the eager
			// optimisation is opportunistic and never blocks Flush.
			if err == nil && n == isize {
				hit, derr := bs.syncer.tryEagerSmallFileDedup(ctx, payloadID, data)
				if derr != nil {
					return nil, fmt.Errorf("eager small-file dedup: %w", derr)
				}
				if hit {
					return &blockstore.FlushResult{Finalized: true}, nil
				}
			}
		}

		// File-level dedup pre-hook: if a fully-quiesced manifest matches
		// an already-stored ObjectID, skip the upload pump entirely.
		specBlocks, blockStates, err := bs.syncer.snapshotPendingBlockRefs(ctx, payloadID)
		if err != nil {
			return nil, fmt.Errorf("snapshot pending blockrefs: %w", err)
		}
		if len(specBlocks) > 0 {
			fileObjectID, err := bs.coordinator.GetFileObjectID(ctx, payloadID)
			if err != nil {
				return nil, fmt.Errorf("get file objectID: %w", err)
			}
			hit, err := bs.syncer.trySpeculativeFileLevelDedup(ctx, payloadID, specBlocks, fileObjectID, blockStates)
			if err != nil {
				return nil, fmt.Errorf("file-level dedup: %w", err)
			}
			if hit {
				return &blockstore.FlushResult{Finalized: true}, nil
			}
		}
	}
	// Delegate to syncer's mirror loop.
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

// LocalForTest returns the engine's underlying local store as the
// local.LocalStore interface. Used by cross-package test fixtures
// (e.g. internal/adapter/common) that need to drive rollup or other
// admin paths against the concrete *fs.FSStore via a type assertion.
// Do not use in production code.
func (bs *BlockStore) LocalForTest() local.LocalStore { return bs.local }

// LocalForTest is the package-level counterpart used when the bs
// receiver is shadowed; mirrors RemoteForTesting on the same wire.
func LocalForTest(bs *BlockStore) local.LocalStore { return bs.local }

// RemoteForTesting returns the remote store for cross-package test verification
// (e.g., shared remote store identity). Do not use in production code.
func (bs *BlockStore) RemoteForTesting() remote.RemoteStore { return bs.remote }

// ListFiles returns the payloadIDs of all files tracked in the local store.
func (bs *BlockStore) ListFiles() []string { return bs.local.ListFiles() }

// EvictLocal removes all local per-file state (memory tracking, files
// map, accessTracker, append log) for a file. CAS chunks are NOT
// removed here — they may be shared with other files via file-level
// dedup and are reclaimed via the refcount → GC path (engine.Delete
// decrements per dropped hash and the mark-sweep GC reaps orphans).
func (bs *BlockStore) EvictLocal(ctx context.Context, payloadID string) error {
	if err := bs.local.EvictMemory(ctx, payloadID); err != nil {
		return err
	}
	return bs.local.DeleteLog(ctx, payloadID)
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
// the local store (with remote-fallback on miss); the cache is hint-only
// and does not serve bytes here.
//
// The primary entry is LocalStore.ReadPayloadAt — a payload-keyed read
// that consults BOTH the in-flight append log (pre-rollup bytes) AND
// the rolled-up CAS chunks via the FileBlock manifest. This closes the
// pre-rollup read-after-write window where freshly-appended bytes would
// otherwise return zeros until the async rollup commits FileBlock rows.
//
// On a local miss (ErrFileBlockNotFound), fall back to the CAS-hash
// walk (readLocalByHash, used for chunks that the manifest knows about
// but the LocalStore did not surface — e.g., post-eviction reads where
// only the metadata row survived) and finally to remote-fetch via the
// syncer.
func (bs *BlockStore) readAtInternal(ctx context.Context, payloadID string, data []byte, offset uint64) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	// Primary: payload-keyed local read. Covers both the pre-rollup
	// append-log window and the post-rollup CAS path.
	n, err := bs.local.ReadPayloadAt(ctx, payloadID, data, offset)
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, blockstore.ErrFileBlockNotFound) {
		return 0, fmt.Errorf("local read failed: %w", err)
	}

	// Local miss — try the CAS-hash walk (handles edge cases where the
	// FileBlockStore manifest is reachable via the engine's
	// fileBlockStore field but not the LocalStore-internal one).
	found, err := bs.readLocalByHash(ctx, payloadID, data, offset)
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

	// Fast path: direct-serve copies S3 data directly to dest, skipping a second read.
	filled, err := bs.syncer.EnsureAvailableAndRead(ctx, payloadID, offset, length, dest)
	if err != nil {
		return fmt.Errorf("direct download failed: %w", err)
	}
	if filled {
		return nil
	}

	found, err := bs.readLocalByHash(ctx, payloadID, dest, offset)
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

// readLocalByHash serves [offset, offset+len(dest)) by walking the
// payload's CAS chunk manifest (via FileBlockStore.ListFileBlocks),
// finding each chunk whose absolute byte range intersects the
// requested window, and copying the matching slice of the local CAS
// chunk into dest. Returns (true, nil) when every requested byte was
// satisfied locally and (false, nil) when any portion of the window
// could not be served from local CAS — the caller treats the false
// outcome as "must fall back to remote-fetch".
//
// On any unexpected error (FileBlock store failure, local chunk store
// I/O error other than ErrChunkNotFound) the function returns
// (false, err) so the engine can surface it to the protocol layer.
//
// Chunk geometry: under the unified CAS surface chunk boundaries are
// FastCDC-derived (variable size, absolute Offset stored on the
// FileBlock row's ID-derived blockIdx slot). The walk is O(N) over
// the per-payload row list — acceptable for the test fixtures (small
// N) and for the steady-state production stream where N is bounded
// by the payload's total size divided by the average chunk size
// (~4 MiB).
func (bs *BlockStore) readLocalByHash(ctx context.Context, payloadID string, dest []byte, offset uint64) (bool, error) {
	if len(dest) == 0 {
		return true, nil
	}
	// The engine consults the same EngineFileBlockStore the syncer
	// uses; ListFileBlocks returns the per-payload row list in
	// blockIdx order, which is offset order under the persister's
	// blockIdx := chunkOffset / BlockSize derivation. Rows missing
	// from the list are sparse: the caller falls back to the
	// remote-fetch + zero-fill path.
	if bs.fileBlockStore == nil {
		return false, nil
	}
	rows, err := bs.fileBlockStore.ListFileBlocks(ctx, payloadID)
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	endOff := offset + uint64(len(dest))
	for currentOffset := offset; currentOffset < endOff; {
		// Find the row whose chunk covers currentOffset. blockIdx is
		// chunkOffset / BlockSize so the chunk's absolute Offset is
		// not directly stored on the row, but we can reconstruct
		// the chunk start by walking rows in order and tracking a
		// running expected offset. For the test fixtures + steady-
		// state production stream chunks land in offset-ascending
		// order, and the row ID's blockIdx component is monotone.
		row := findRowCoveringOffset(rows, currentOffset)
		if row == nil || row.fb.Hash.IsZero() {
			return false, nil
		}
		data, err := bs.local.Get(ctx, row.fb.Hash)
		if err != nil {
			if errors.Is(err, blockstore.ErrChunkNotFound) {
				return false, nil
			}
			return false, err
		}
		// Clamp the visible data to FileBlock.DataSize so a padded
		// on-disk chunk surface doesn't leak garbage past the
		// rollup-emitted byte count.
		dataLen := uint64(len(data))
		if uint64(row.fb.DataSize) > 0 && uint64(row.fb.DataSize) < dataLen {
			dataLen = uint64(row.fb.DataSize)
		}
		chunkAbsEnd := row.absOffset + dataLen
		if currentOffset >= chunkAbsEnd {
			// Should not happen if findRowCoveringOffset returned
			// a row covering currentOffset — surface as sparse and
			// let the caller fall back.
			return false, nil
		}
		srcOff := currentOffset - row.absOffset
		copyLen := chunkAbsEnd - currentOffset
		if copyLen > endOff-currentOffset {
			copyLen = endOff - currentOffset
		}
		copy(dest[currentOffset-offset:currentOffset-offset+copyLen], data[srcOff:srcOff+copyLen])
		currentOffset += copyLen
	}
	return true, nil
}

// rowWithOffset bundles a FileBlock row with the absolute payload
// offset of its first byte. The persister encodes the chunk's
// absolute offset directly as the numeric component of the row ID
// ("<payloadID>/<chunkOffset>"), so absOffset is the parsed
// component verbatim.
type rowWithOffset struct {
	fb        *blockstore.FileBlock
	absOffset uint64
}

// findRowCoveringOffset returns the row whose absolute byte range
// [absOffset, absOffset+DataSize) contains target, or nil if no row
// in rows covers it. The walk is O(N) over the per-payload row
// list — acceptable for the FastCDC steady-state (chunks average ~4 MiB
// so even a 4 GiB file produces ~1000 rows).
func findRowCoveringOffset(rows []*blockstore.FileBlock, target uint64) *rowWithOffset {
	for _, fb := range rows {
		if fb == nil {
			continue
		}
		abs, ok := parseChunkOffsetFromID(fb.ID)
		if !ok {
			continue
		}
		if target >= abs && target < abs+uint64(fb.DataSize) {
			return &rowWithOffset{fb: fb, absOffset: abs}
		}
	}
	return nil
}

// parseChunkOffsetFromID extracts the trailing numeric component of a
// FileBlock ID of the form "<payloadID>/<chunkOffset>" and returns
// (chunkOffset, true) on success. Returns (0, false) for malformed
// IDs.
func parseChunkOffsetFromID(id string) (uint64, bool) {
	slash := -1
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '/' {
			slash = i
			break
		}
	}
	if slash < 0 || slash == len(id)-1 {
		return 0, false
	}
	var v uint64
	for _, c := range id[slash+1:] {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + uint64(c-'0')
	}
	return v, true
}
