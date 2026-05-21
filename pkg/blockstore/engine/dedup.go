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
			// retry once. For hashes shared between original and
			// updated targets, this performs two metadata RTTs (one
			// decrement here, one re-increment in the retry) — that is
			// the documented tradeoff. The simpler "decrement
			// everything, retry from scratch" pattern is correct under
			// the per-call refcount-accounting model: each
			// applyFileLevelDedupHit call must own exactly one
			// increment per target hash by the time it returns
			// success, so partial rollbacks risk double-counting on
			// the retry. WR-01 (Phase 13 review iteration 1):
			// considered the symmetric-difference optimisation but
			// rejected it because the retry unconditionally
			// re-increments every updatedTarget hash; skipping shared
			// hashes here would leave them at +2 after retry instead
			// of the intended +1. INV-02 audit reconciles any
			// transient dip while the retry runs (RefCount is
			// non-blocking; GC keys off FileAttr.Blocks references in
			// the persisted state, not the in-flight rollback window).
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
	//
	// WR-02 (Phase 13 review iteration 1): retry-time invariant. On
	// the retry path (isRetry=true) targetBlocks is the updated
	// canonical target's slice (passed via the recursive call after
	// FindByObjectID); seen is the recursive frame's freshly-built
	// set computed from those same targetBlocks (step 1 above).
	// speculativeBlocks is invariant across retry — it is the engine's
	// chunker output, not derived from any per-call lookup. Therefore
	// targetSet (recomputed each call from targetBlocks) and
	// speculativeSet are correct for whichever call frame is
	// executing. If a future refactor introduces partial speculation
	// (chunker emitting different blocks during retry) the
	// speculativeSet computation MUST move to
	// trySpeculativeFileLevelDedup so it cannot drift across retries.
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
	// pattern) are observed; bs.cache is never nil thanks to the Null
	// Object pattern installed by engine.New.
	if m.bs != nil {
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
		if err := m.local.DeleteLog(ctx, payloadID); err != nil {
			logger.Warn("file-level dedup: delete append log",
				"payloadID", payloadID, "err", err)
		}
	}

	// 6. WR-02 (Phase 13 review iteration 2): purge speculative FileBlock
	// rows for payloadID. After step 2 PersistFileBlocks succeeded,
	// FileAttr.Blocks points to the target's BlockRefs and reads resolve
	// via target's hashes (GetByHash routes to the target's persisted
	// row, whose RefCount we just incremented). The local speculative
	// rows under "{payloadID}/{idx}" are now orphans:
	//
	//   - They are still Pending (the upload pump was bypassed) so the
	//     periodic mirror loop would surface them via ListUnsynced and
	//     re-Put them on the next tick — wasted bandwidth even with
	//     CAS idempotency, and a NEW PUT on first hit.
	//   - A subsequent Flush(payloadID) per-block drain path would feed
	//     them into snapshotBlockRefs after the periodic uploader marked
	//     them Remote, computing a Merkle root from speculative content
	//     and silently overwriting the target-sourced FileAttr.Blocks /
	//     ObjectID — reverting the dedup hit's atomic swap.
	//   - Syncer.GetFileSize / Exists consult ListFileBlocks(payloadID);
	//     a speculative row reaching Remote could diverge from
	//     FileAttr.Size / target-derived size.
	//
	// Speculative-row IDs ("{payloadID}/{idx}") and target-row IDs
	// ("{target_payloadID}/{idx}") are disjoint by payloadID prefix, so
	// no filter against target's projection is needed — every row in
	// ListFileBlocks(payloadID) at this point is a speculative orphan.
	//
	// Best-effort: a failure here leaves orphan Pending rows that the
	// periodic uploader will eventually resurface, but the metadata
	// commit (step 2) has already swapped FileAttr.Blocks; reads remain
	// correct. The next successful Flush will re-attempt cleanup via
	// this same path. Logging at WARN matches the speculative-only
	// refcount decrement contract above (orphan; periodic janitor or
	// future quiesce reclaims).
	if m.fileBlockStore != nil {
		specRows, lerr := m.fileBlockStore.ListFileBlocks(ctx, payloadID)
		if lerr != nil {
			logger.Warn("file-level dedup: list speculative FileBlocks for cleanup",
				"payloadID", payloadID, "err", lerr)
		} else {
			for _, fb := range specRows {
				if derr := m.fileBlockStore.Delete(ctx, fb.ID); derr != nil {
					logger.Warn("file-level dedup: delete speculative FileBlock row",
						"blockID", fb.ID, "err", derr)
				}
			}
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
		// Post-Phase-17: no per-file prefix delete on the renamed
		// RemoteStore. Without a FileBlockStore we have no BlockRef
		// list to decrement RefCount against — the deletion is a no-op
		// and GC reclaims any orphans.
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

			// Defensive: post-Phase-17 every reachable FileBlock is
			// CAS-shaped (non-zero Hash). A stale zero-hash row
			// pre-dating migration would be a bug; skip the remote
			// delete rather than panic on the empty hash.
			if !fb.Hash.IsZero() && m.remoteStore != nil {
				if err := m.remoteStore.Delete(ctx, fb.Hash); err != nil {
					logger.Warn("Failed to delete block from store",
						"blockID", blockID,
						"error", err)
				}
			} else if fb.Hash.IsZero() {
				logger.Error("DeleteWithRefCount: zero-hash FileBlock encountered post-migration; skipping remote delete",
					"blockID", blockID)
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
