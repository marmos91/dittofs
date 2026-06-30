// Package engine — block-aware GC reclaim (#1414 object packing, PR3).
//
// A packed-block chunk lives inside blocks/<blockID> rather than as a standalone
// cas/<hash> object. Its bytes are shared with the other chunks in the same
// block, so the block object can be reclaimed only when its LAST live chunk is
// gone. Reclaim is driven by the existing remote GC sweep — NOT by the unlink
// refcount cascade — because only the sweep's live-set scan proves a hash is
// globally dead (no sibling FileChunk row, in any file, still references it).
// Deciding to free a block on a single file's unlink would corrupt a dedup
// sibling that shares the content; the sweep reaches a hash only after that
// hazard is excluded, so DecrLiveChunkCount here can never race a live sibling.
package engine

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
)

// BlockReclaimer reclaims a globally-dead chunk that the remote GC sweep has
// proven unreferenced. It is consulted ONCE per swept hash, in place of the
// standalone cas/<hash> Delete: a block-resident hash has no CAS object, so the
// reclaimer decrements its enclosing block and frees the block object + record
// when the last live chunk is gone. A nil Options.BlockReclaimer means the
// deployment packs no blocks — the sweep deletes cas/<hash> objects exactly as
// before, so non-block deployments are unaffected.
type BlockReclaimer interface {
	// ReclaimDeadChunk handles a globally-dead chunk hash. It returns
	// handled=true (with the remote bytes freed, if any) when the hash was
	// block-resident and the block bookkeeping was applied — the caller MUST
	// then skip the standalone CAS delete (no such object exists). handled=false
	// means the hash is a standalone CAS object and the caller deletes it as
	// before. Idempotent: a hash whose block was already freed by a sibling
	// chunk in the same sweep returns handled=true with zero bytes.
	ReclaimDeadChunk(ctx context.Context, hash block.ContentHash) (handled bool, bytesFreed int64, err error)
}

// blockLocatorResolver resolves a chunk hash to its remote locator. Satisfied by
// the per-share metadata store (metadata.SyncedHashStore.GetLocator).
type blockLocatorResolver interface {
	GetLocator(ctx context.Context, hash block.ContentHash) (block.ChunkLocator, bool, error)
}

// blockRecordGC is the block-record bookkeeping the reclaimer mutates. Satisfied
// by the per-share metadata store (metadata.BlockRecordStore subset).
type blockRecordGC interface {
	GetBlockRecord(ctx context.Context, blockID string) (block.BlockRecord, bool, error)
	DecrLiveChunkCount(ctx context.Context, blockID string, delta uint32) (uint32, error)
	DeleteBlockRecord(ctx context.Context, blockID string) error
}

// localChunkDeleter drops a chunk's local-index entry. Satisfied by the
// per-share metadata store (metadata.LocalChunkIndex.DeleteLocalLocation).
type localChunkDeleter interface {
	DeleteLocalLocation(ctx context.Context, hash block.ContentHash) error
}

// BlockGCReclaimer is the reference BlockReclaimer the remote GC sweep uses to
// reclaim block-resident chunks. It binds the per-share metadata store's
// locator/record/local-index surfaces and the block-keyed remote store. The
// runtime constructs one per remote-store sweep scope and sets it on
// engine.Options.BlockReclaimer.
type BlockGCReclaimer struct {
	Locators     blockLocatorResolver
	Records      blockRecordGC
	LocalIndex   localChunkDeleter
	RemoteBlocks remote.RemoteBlockStore
}

// ReclaimDeadChunk implements BlockReclaimer. See the interface contract.
//
// Ordering is chosen for crash-safety. DecrLiveChunkCount runs BEFORE
// DeleteLocalLocation so a crash between them leaves an orphan local-index entry
// (harmless; a local reconcile reclaims it) rather than an over-counted block
// that never floors to zero (a permanent remote leak). The remote DeleteBlock
// precedes DeleteBlockRecord so a crash between them leaves a record-less orphan
// object (reclaimed by the deferred orphan-object sweep) rather than a record
// pointing at deleted bytes.
func (r *BlockGCReclaimer) ReclaimDeadChunk(ctx context.Context, hash block.ContentHash) (bool, int64, error) {
	loc, synced, err := r.Locators.GetLocator(ctx, hash)
	if err != nil {
		return false, 0, fmt.Errorf("block reclaim: get locator %s: %w", hash, err)
	}
	if !synced || loc.IsStandalone() {
		// Standalone CAS object (or unsynced): the caller deletes cas/<hash>.
		return false, 0, nil
	}
	blockID := loc.BlockID

	// Existence + Length up front: a missing record means the block was already
	// freed by a sibling chunk earlier in this sweep (idempotent re-entry).
	rec, ok, err := r.Records.GetBlockRecord(ctx, blockID)
	if err != nil {
		return false, 0, fmt.Errorf("block reclaim: get record %s: %w", blockID, err)
	}
	if !ok {
		// Orphan locator pointing at an already-freed block: drop its local entry
		// and report handled so the caller skips the (nonexistent) CAS delete.
		if derr := r.LocalIndex.DeleteLocalLocation(ctx, hash); derr != nil {
			return false, 0, fmt.Errorf("block reclaim: delete local location %s: %w", hash, derr)
		}
		return true, 0, nil
	}

	remaining, err := r.Records.DecrLiveChunkCount(ctx, blockID, 1)
	if err != nil {
		return false, 0, fmt.Errorf("block reclaim: decr live chunk count %s: %w", blockID, err)
	}
	if derr := r.LocalIndex.DeleteLocalLocation(ctx, hash); derr != nil {
		return false, 0, fmt.Errorf("block reclaim: delete local location %s: %w", hash, derr)
	}
	if remaining > 0 {
		return true, 0, nil // block still has live chunks — keep it
	}

	// Last live chunk gone: free the remote object, then the record.
	if derr := r.RemoteBlocks.DeleteBlock(ctx, blockID); derr != nil {
		return false, 0, fmt.Errorf("block reclaim: delete block %s: %w", blockID, derr)
	}
	if derr := r.Records.DeleteBlockRecord(ctx, blockID); derr != nil {
		return false, 0, fmt.Errorf("block reclaim: delete record %s: %w", blockID, derr)
	}
	return true, rec.Length, nil
}

// ensure compile-time interface satisfaction.
var _ BlockReclaimer = (*BlockGCReclaimer)(nil)
