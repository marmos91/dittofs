package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
)

// trySpeculativeFileLevelDedup is the BSCAS-05 file-level dedup
// short-circuit entry point (Phase 13 D-08 / D-09 / D-10 / D-11 / D-14).
//
// Trigger condition (D-09):
//
//	len(speculativeBlocks) > 0 AND
//	every blockStates[i] == blockstore.BlockStatePending AND
//	fileObjectID == zero (file never quiesced)
//
// Note (W-6): chunker output is by construction Pending. The
// blockStates parameter is retained for explicit invariant checking
// and to keep parity with Plan 06's RED test signature; callers MUST
// only invoke this with the chunker's freshly-emitted blocks where the
// invariant holds.
//
// On hit (D-10): in one metadata txn (caller-owned)
//   - IncrementRefCount on every distinct hash in target.Blocks.
//   - PersistFileBlocks(payloadID, target.Blocks, provisionalObjectID).
//   - Best-effort DecrementRefCount on speculative-only hashes.
//   - Cache.InvalidateFile for orphaned speculative chunks.
//   - DeleteAppendLog for the per-file log (D-11).
//
// On race-loser (D-14): PersistFileBlocks returns ErrObjectIDConflict.
// We re-call FindByObjectID, decrement RefCount on our just-incremented
// target hashes, swap to the now-canonical target's BlockRef list, and
// re-call PersistFileBlocks. Single retry; further conflicts propagate.
//
// Returns (true, nil) on hit, (false, nil) on miss (caller proceeds
// with the existing per-block GetByHash + WriteBlockWithHash path),
// (false, err) on a backend error that should propagate.
func (m *Syncer) trySpeculativeFileLevelDedup(
	ctx context.Context,
	payloadID string,
	speculativeBlocks []blockstore.BlockRef,
	fileObjectID blockstore.ObjectID,
	blockStates []blockstore.BlockState,
) (hit bool, err error) {
	if m.coordinator == nil {
		return false, nil
	}
	// D-09 trigger condition.
	if len(speculativeBlocks) == 0 {
		return false, nil
	}
	if !fileObjectID.IsZero() {
		return false, nil
	}
	for _, st := range blockStates {
		if st != blockstore.BlockStatePending {
			return false, nil
		}
	}

	provisional := blockstore.ComputeObjectID(speculativeBlocks)

	targetBlocks, err := m.coordinator.FindByObjectID(ctx, provisional)
	if err != nil {
		return false, err
	}
	if targetBlocks == nil {
		// Miss — caller continues per-block path; ObjectID is finalized
		// at the existing post-Flush coordinator hook.
		return false, nil
	}

	return m.applyFileLevelDedupHit(ctx, payloadID, speculativeBlocks, targetBlocks, provisional, false /*isRetry*/)
}

// isObjectIDConflict reports whether err signals a Phase 13 D-14
// first-committer-wins concurrent-quiesce race. Recognises two shapes:
//
//  1. errors.Is(err, ErrObjectIDConflict) — the runtime coordinator
//     wraps backend conflict errors (Postgres 23505 on
//     files_object_id_idx, mderrors.ErrConflict from Memory/Badger)
//     into this sentinel via errors.Join.
//  2. duck-typed `interface{ IsConflict() bool }` — accepted as a
//     compatibility hook so test fakes (and any future low-level
//     driver type that surfaces the same boolean) can flow through
//     without coupling test code to the engine sentinel.
//
// Either signal triggers the single-shot retry path in
// applyFileLevelDedupHit.
func isObjectIDConflict(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrObjectIDConflict) {
		return true
	}
	type conflictSignal interface {
		IsConflict() bool
	}
	var sig conflictSignal
	if errors.As(err, &sig) && sig.IsConflict() {
		return true
	}
	return false
}

// applyFileLevelDedupHit performs the metadata-side swap once
// FindByObjectID has confirmed a file with identical Merkle root
// already exists in the metadata store. See trySpeculativeFileLevelDedup
// for the higher-level contract; this helper is also re-entered once
// (with isRetry=true) to absorb the D-14 first-committer-wins race.
func (m *Syncer) applyFileLevelDedupHit(
	ctx context.Context,
	payloadID string,
	speculativeBlocks []blockstore.BlockRef,
	targetBlocks []blockstore.BlockRef,
	provisional blockstore.ObjectID,
	isRetry bool,
) (bool, error) {
	// 1. Increment RefCount on every distinct hash in target.
	seen := make(map[blockstore.ContentHash]struct{}, len(targetBlocks))
	for _, br := range targetBlocks {
		if _, ok := seen[br.Hash]; ok {
			continue
		}
		seen[br.Hash] = struct{}{}
		if err := m.coordinator.IncrementRefCount(ctx, br.Hash); err != nil {
			return false, fmt.Errorf("file-level dedup: increment refcount on target hash %s: %w", br.Hash.String(), err)
		}
	}

	// 2. Persist target.Blocks + provisional ObjectID (single metadata
	// txn, caller-owned).
	err := m.coordinator.PersistFileBlocks(ctx, payloadID, targetBlocks, provisional)
	if err != nil {
		if !isRetry && isObjectIDConflict(err) {
			// Concurrent-quiesce race (D-14): someone else committed
			// first. Roll back our just-incremented refcounts on the
			// original target, re-fetch the now-canonical target, and
			// retry once.
			for h := range seen {
				if _, derr := m.coordinator.DecrementRefCount(ctx, h); derr != nil {
					logger.Warn("file-level dedup race rollback decrement failed",
						"hash", h.String(), "err", derr)
				}
			}
			updatedTarget, ferr := m.coordinator.FindByObjectID(ctx, provisional)
			if ferr != nil {
				return false, fmt.Errorf("file-level dedup race: re-lookup target: %w", ferr)
			}
			if updatedTarget == nil {
				return false, fmt.Errorf("file-level dedup race: target vanished after conflict on objectID %s", provisional.String())
			}
			return m.applyFileLevelDedupHit(ctx, payloadID, speculativeBlocks, updatedTarget, provisional, true /*isRetry*/)
		}
		// Best-effort rollback of refcount increments on a non-conflict error.
		for h := range seen {
			if _, derr := m.coordinator.DecrementRefCount(ctx, h); derr != nil {
				logger.Warn("file-level dedup hit rollback decrement failed",
					"hash", h.String(), "err", derr)
			}
		}
		return false, fmt.Errorf("file-level dedup persist: %w", err)
	}

	// 3. Decrement RefCount on speculative-only hashes (D-10 step 4).
	//
	// W-5: this step is BEST-EFFORT. Step 2's PersistFileBlocks has
	// already committed the swap to target's BlockRef list. Failures
	// here do NOT roll back the persisted state — they leave orphaned
	// refcount entries that the GC sweep (Phase 11 GC-01..04) will
	// reclaim. Logging at WARN matches that contract (orphan; GC will
	// reclaim).
	targetSet := make(map[blockstore.ContentHash]struct{}, len(targetBlocks))
	for _, br := range targetBlocks {
		targetSet[br.Hash] = struct{}{}
	}
	speculativeSet := make(map[blockstore.ContentHash]struct{}, len(speculativeBlocks))
	for _, br := range speculativeBlocks {
		speculativeSet[br.Hash] = struct{}{}
	}
	for h := range speculativeSet {
		if _, ok := targetSet[h]; ok {
			continue
		}
		if _, err := m.coordinator.DecrementRefCount(ctx, h); err != nil {
			logger.Warn("file-level dedup: decrement speculative-only refcount (orphan; GC will reclaim)",
				"hash", h.String(), "err", err)
		}
	}

	// 4. Cache invalidation for orphaned speculative chunks (D-10 step
	// 5 / Phase 12 D-35). Build the removed-hash list in BlockRef order
	// (preserves multiplicity expectations of the surgical-invalidate
	// contract). Read through the BlockStore back-reference so that
	// post-construction `bs.cache = rec` swaps (TestClose_ClosesCache
	// pattern) are observed.
	if m.bs != nil && m.bs.cache != nil {
		removed := make([]blockstore.ContentHash, 0, len(speculativeBlocks))
		for _, br := range speculativeBlocks {
			if _, ok := targetSet[br.Hash]; !ok {
				removed = append(removed, br.Hash)
			}
		}
		m.bs.cache.InvalidateFile(payloadID, removed)
	}

	// 5. Per-file append-log truncation (D-11). Best-effort: a failure
	// here leaves a stale append log behind that the next quiesce will
	// rewrite, but the metadata commit has already happened and reads
	// will resolve via target's BlockRefs.
	if m.local != nil {
		if err := m.local.DeleteAppendLog(ctx, payloadID); err != nil {
			logger.Warn("file-level dedup: delete append log",
				"payloadID", payloadID, "err", err)
		}
	}

	logger.Debug("file-level dedup short-circuit hit",
		"payloadID", payloadID,
		"objectID", provisional.String(),
		"donor_blocks", len(targetBlocks),
		"speculative_blocks", len(speculativeBlocks),
		"is_retry", isRetry)

	return true, nil
}

// DeleteWithRefCount decrements RefCount for each block and deletes blocks that reach zero.
func (m *Syncer) DeleteWithRefCount(ctx context.Context, payloadID string, blockIDs []string) error {
	if !m.canProcess(ctx) {
		return ErrClosed
	}

	if m.fileBlockStore == nil {
		if m.remoteStore != nil {
			return m.remoteStore.DeleteByPrefix(ctx, payloadID+"/")
		}
		return nil
	}

	for _, blockID := range blockIDs {
		newCount, err := m.fileBlockStore.DecrementRefCount(ctx, blockID)
		if err != nil {
			logger.Warn("Failed to decrement block refcount",
				"blockID", blockID, "error", err)
			continue
		}

		if newCount == 0 {
			fb, err := m.fileBlockStore.GetFileBlock(ctx, blockID)
			if err != nil {
				continue
			}

			if fb.BlockStoreKey != "" && m.remoteStore != nil {
				if err := m.remoteStore.DeleteBlock(ctx, fb.BlockStoreKey); err != nil {
					logger.Warn("Failed to delete block from store",
						"blockID", blockID,
						"error", err)
				}
			}

			if err := m.fileBlockStore.Delete(ctx, blockID); err != nil {
				logger.Warn("Failed to delete block metadata",
					"blockID", blockID,
					"error", err)
			}
		}
	}

	return nil
}
