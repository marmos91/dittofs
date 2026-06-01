package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
)

// ReadAt reads data from storage at the given offset into dest.
// a non-nil/non-empty []BlockRef carries the CAS hashes covering the
// requested range (zero-filling sparse holes).
//
// After a successful read the engine calls cache.OnRead(payloadID
// blockHashes, fileSize) so the Cache's sequential-detection state
// machine can fire prefetch on upcoming hashes. The cache is hint-only
// here; reads always go through local/remote stores.
func (bs *Store) ReadAt(ctx context.Context, payloadID string, blocks []blockstore.BlockRef, data []byte, offset uint64) (int, error) {
	if err := bs.enter(); err != nil {
		return 0, err
	}
	defer bs.closeMu.RUnlock()
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

// GetSize returns the stored size of a payload.
// Checks local store first, falls back to syncer (remote).
func (bs *Store) GetSize(ctx context.Context, payloadID string) (uint64, error) {
	if err := bs.enter(); err != nil {
		return 0, err
	}
	defer bs.closeMu.RUnlock()
	if size, found := bs.local.GetFileSize(ctx, payloadID); found {
		return size, nil
	}
	return bs.syncer.GetFileSize(ctx, payloadID)
}

// Exists checks whether a payload exists.
// Checks local store first, falls back to syncer (remote).
func (bs *Store) Exists(ctx context.Context, payloadID string) (bool, error) {
	if err := bs.enter(); err != nil {
		return false, err
	}
	defer bs.closeMu.RUnlock()
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
// signature returns []BlockRef so the caller can persist
// FileAttr.Blocks in the same metadata txn.
//
// WriteAt remains a per-write append into the local store — it does
// NOT chunk or assemble BlockRefs. The FastCDC chunker runs at the
// local-store rollup layer (pkg/blockstore/local/fs/rollup.go
// rollupFile), which produces Pending FileBlocks carrying chunk
// hashes. Syncer.Flush projects ListFileBlocks(payloadID) into the
// canonical sorted []BlockRef list at quiesce time and invokes either
// the file-level dedup short-circuit or the per-block
// upload pump + post-Flush hook. FileAttr.Blocks
// AND FileAttr.ObjectID are written in the same metadata transaction
// by the runtime coordinator's PersistFileBlocks.
//
// Returns currentBlocks unchanged — the canonical projection happens
// at Flush time, not WriteAt time.
func (bs *Store) WriteAt(ctx context.Context, payloadID string, currentBlocks []blockstore.BlockRef, data []byte, offset uint64) ([]blockstore.BlockRef, error) {
	if err := bs.enter(); err != nil {
		return currentBlocks, err
	}
	defer bs.closeMu.RUnlock()
	if len(data) == 0 {
		return currentBlocks, nil
	}
	if err := bs.local.AppendWrite(ctx, payloadID, data, offset); err != nil {
		return currentBlocks, err
	}
	// Cache invalidation lives in common.WriteToBlockStore (post-txn)
	// not here. The engine itself does NOT touch cache on the write
	// path beyond resetting the per-payload sequential tracker via
	// OnRead's empty-hashes signal — keeps prefetch from chasing
	// pre-write hashes after the underlying data shifted. nullCache is
	// a no-op (Null Object).
	bs.cache.OnRead(payloadID, nil, 0)
	// the FastCDC chunker output is
	// produced by the local-store rollup pump
	// (pkg/blockstore/local/fs/rollup.go:rollupFile) and lands as
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
// when currentBlocks is non-empty, blocks strictly past newSize
// are dropped and the coordinator decrements RefCount for each dropped
// hash. The new []BlockRef list is returned for the caller to persist
// via PutFile. When currentBlocks is empty the legacy path runs and
// the returned slice is empty (dual-read shim semantics).
func (bs *Store) Truncate(ctx context.Context, payloadID string, currentBlocks []blockstore.BlockRef, newSize uint64) ([]blockstore.BlockRef, error) {
	if err := bs.enter(); err != nil {
		return currentBlocks, err
	}
	defer bs.closeMu.RUnlock()
	// coordinator decrements run FIRST so a refcount-bookkeeping
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
		var dropped []blockstore.BlockRef
		for _, b := range currentBlocks {
			if b.Offset >= newSize {
				// Block fully past newSize — drop it.
				dropped = append(dropped, b)
				continue
			}
			// Block fully or partially before newSize — keep. WriteAt will
			// re-chunk the partial-tail block; keep it as-is.
			kept = append(kept, b)
		}

		// Reap each dropped tail hash so it leaves EnumerateFileBlocks and the
		// GC sweep can reclaim the remote chunk (otherwise truncated tail
		// chunks leak on the remote forever — #832).
		//
		// RefCount is incremented ONCE per DISTINCT hash per file (file-level
		// dedup uses a seen-set in applyFileLevelDedupHit / CopyPayload), so
		// decrement at most once per distinct dropped hash — and never for a
		// hash still referenced by a KEPT block (the same content can sit on
		// both sides of newSize). A per-BlockRef decrement would over-drop a
		// shared row to zero and let GC delete data another reference needs.
		if bs.coordinator != nil {
			keptHashes := make(map[blockstore.ContentHash]struct{}, len(kept))
			for _, b := range kept {
				keptHashes[b.Hash] = struct{}{}
			}
			reaped := make(map[blockstore.ContentHash]struct{}, len(dropped))
			for _, b := range dropped {
				if _, stillKept := keptHashes[b.Hash]; stillKept {
					continue
				}
				if _, done := reaped[b.Hash]; done {
					continue
				}
				reaped[b.Hash] = struct{}{}
				if _, err := bs.coordinator.DecrementRefCountAndReap(ctx, b.Hash); err != nil {
					return currentBlocks, fmt.Errorf("decrement refcount on truncate-drop %s: %w", b.Hash.String(), err)
				}
			}
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
	// engine.Delete semantics post-).
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
// Local cleanup runs in this order under the unified CAS surface
//  1. SyncFileBlocksForFile persists any in-flight FileBlock metadata so
//     the refcount decrements below operate on the authoritative manifest
//     for the file (see "blocks" arg).
//  2. EvictMemory drops the per-file in-memory tracking (memBlocks, files
//     map, accessTracker entry). There are no legacy per-file block files
//     to remove — the CAS chunk store under blocks/<hh>/ is the only
//     on-disk layout, and individual chunks are reclaimed via refcount →
//     GC, not per-file enumeration.
//  3. DeleteAppendLog tombstones and removes the per-file append log so any
//     pre-rollup bytes are discarded.
//
// Subsequent steps (cache invalidate, coordinator refcount decrements
// optional remote sweep) are unchanged.
func (bs *Store) Delete(ctx context.Context, payloadID string, blocks []blockstore.BlockRef) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	bs.local.SyncFileBlocksForFile(ctx, payloadID)
	if err := bs.local.EvictMemory(ctx, payloadID); err != nil {
		return fmt.Errorf("local evict memory failed: %w", err)
	}
	if err := bs.local.DeleteAppendLog(ctx, payloadID); err != nil {
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
	// continue past coordinator errors so the syncer.Delete
	// remote sweep ALWAYS runs. Returning early left the local data
	// deleted, the metadata partially decremented, and the remote alive
	// forever — operators saw inconsistent state until GC's next pass
	// (hours). Now we capture the first coordinator error, finish
	// decrementing the rest, run the remote sweep unconditionally, and
	// return errors.Join of both surfaces so the caller sees the full
	// picture.
	var coordErr error
	if len(blocks) > 0 && bs.coordinator != nil {
		// RefCount is incremented ONCE per DISTINCT hash per file (file-level
		// dedup uses a seen-set in applyFileLevelDedupHit / CopyPayload), so
		// reap at most once per distinct hash. A per-BlockRef decrement would
		// over-drop a hash that appears at multiple offsets in this file and,
		// when another file shares it, take the shared row to zero and let GC
		// delete data the other reference still needs.
		reaped := make(map[blockstore.ContentHash]struct{}, len(blocks))
		for _, b := range blocks {
			if _, done := reaped[b.Hash]; done {
				continue
			}
			reaped[b.Hash] = struct{}{}
			// Reap at RefCount 0 so the hash leaves EnumerateFileBlocks and
			// the GC sweep can reclaim the remote chunk (#832). Dedup-shared
			// hashes (RefCount > 1) survive — only the last reference reaps.
			newCount, err := bs.coordinator.DecrementRefCountAndReap(ctx, b.Hash)
			if err != nil {
				if coordErr == nil {
					coordErr = fmt.Errorf("decrement refcount on delete %s: %w", b.Hash.String(), err)
				}
				continue
			}
			// Refcount hit zero: the local CAS chunk is being reclaimed
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
// CopyPayload data-copy semantics are removed in; the legacy
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
func (bs *Store) CopyPayload(ctx context.Context, srcPayloadID, dstPayloadID string, srcBlocks []blockstore.BlockRef) ([]blockstore.BlockRef, error) {
	if err := bs.enter(); err != nil {
		return nil, err
	}
	defer bs.closeMu.RUnlock()
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
