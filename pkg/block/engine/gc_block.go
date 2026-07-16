// Package engine — block-aware GC reclaim (#1414 object packing, PR3).
//
// A chunk lives inside a packed blocks/<blockID> object. Its bytes are shared
// with the other chunks in the same block, so the block object can be
// reclaimed only when its LAST live chunk is gone. Reclaim is driven by the
// existing remote GC sweep — NOT by the unlink
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
// proven unreferenced. It is consulted ONCE per swept hash and is the ONLY
// remote reclaim path: the reclaimer decrements the chunk's enclosing block
// and frees the block object + record when the last live chunk is gone.
type BlockReclaimer interface {
	// ReclaimDeadChunk handles a globally-dead chunk hash. It returns
	// handled=true (with the remote bytes freed, if any) when the hash
	// resolved to a block locator and the block bookkeeping was applied.
	// handled=false means this reclaimer has no block locator for the hash —
	// post-#1493 the caller treats that as metadata drift and keeps the
	// marker (fail-closed); it never issues a per-hash remote delete.
	// Idempotent: a hash whose block was already freed by a sibling chunk in
	// the same sweep returns handled=true with zero bytes.
	ReclaimDeadChunk(ctx context.Context, hash block.ContentHash) (handled bool, bytesFreed int64, err error)
}

// blockSyncedMarkerGC is the synced-marker surface the reclaimer needs: resolve
// a chunk hash to its remote locator, then clear the marker. The marker doubles
// as the decrement's per-hash idempotency token (see ReclaimDeadChunk). Satisfied
// by the per-share metadata store (metadata.SyncedHashStore).
type blockSyncedMarkerGC interface {
	GetLocator(ctx context.Context, hash block.ContentHash) (block.ChunkLocator, bool, error)
	DeleteSynced(ctx context.Context, hash block.ContentHash) error
}

// blockRecordGC is the block-record bookkeeping the reclaimer mutates. Satisfied
// by the per-share metadata store (metadata.BlockRecordStore subset).
type blockRecordGC interface {
	GetBlockRecord(ctx context.Context, blockID string) (block.BlockRecord, bool, error)
	DecrLiveChunkCount(ctx context.Context, blockID string, delta uint32) (uint32, error)
	DeleteBlockRecord(ctx context.Context, blockID string) error
}

// BlockGCReclaimer is the reference BlockReclaimer the remote GC sweep uses to
// reclaim block-resident chunks. It binds the per-share metadata store's
// locator/record surfaces and the block-keyed remote store. The runtime
// constructs one per remote-store sweep scope and sets it on
// engine.Options.BlockReclaimer.
type BlockGCReclaimer struct {
	Locators     blockSyncedMarkerGC
	Records      blockRecordGC
	RemoteBlocks remote.RemoteBlockStore
}

// ReclaimDeadChunk implements BlockReclaimer. See the interface contract.
//
// Ordering is chosen for crash-safety and exactly-once decrement semantics. The
// SYNCED MARKER — not the local-index entry — is the decrement's per-hash
// idempotency token: once cleared, GetLocator reports the hash unsynced on any
// re-visit and this reclaimer is a no-op, so DecrLiveChunkCount can never run
// twice for the same hash. The marker is the token because EVICTION drops a
// chunk's local-index entry WITHOUT decrementing (pkg/block/local/fs/eviction.go
// dropBlobIndexEntries), so under the old local-index token an evicted-then-
// orphaned chunk looked "already reclaimed", its decrement was skipped, and its
// block leaked forever (#1637). The marker is untouched by eviction.
//
// The marker is cleared at one of two points, keyed off whether this is the
// block's LAST live chunk (its record count is about to floor to 0):
//
//   - NOT the last chunk (a live sibling remains): clear the marker BEFORE the
//     decrement. A double decrement here would drop LiveChunkCount below the
//     true live count and could free a block a live dedup sibling still needs
//     (data loss), so the marker must gate the decrement. A crash after the
//     clear but before the decrement fails toward over-count: a record with no
//     live locator and a stale count > 0 — a class-2 "leaked" record the
//     reconcile sweep reaps (reclaim.go). Never toward a premature free.
//
//   - the LAST chunk (count floors to 0): clear the marker LAST, after the
//     remote object and record are freed. A re-visit before that re-decrements
//     0 → 0 (harmless floor), so gating is unnecessary; deferring the clear lets
//     a transient DeleteBlock failure keep the marker so the next sweep retries.
//
// DeleteBlock precedes DeleteBlockRecord so a crash between them leaves a
// record-less orphan object (class 3, reclaimed by the deferred orphan-object
// sweep) rather than a record pointing at deleted bytes.
func (r *BlockGCReclaimer) ReclaimDeadChunk(ctx context.Context, hash block.ContentHash) (bool, int64, error) {
	loc, synced, err := r.Locators.GetLocator(ctx, hash)
	if err != nil {
		return false, 0, fmt.Errorf("block reclaim: get locator %s: %w", hash, err)
	}
	if !synced || loc.BlockID == "" {
		// Either no block locator recorded in THIS share's store (another share
		// on the same remote may still resolve it — the runtime unions per-share
		// reclaimers; if none does, the caller records the drift and keeps the
		// marker fail-closed), OR a crash-recovery re-visit whose marker this
		// reclaimer already cleared alongside its committed decrement. Nothing
		// left to reclaim here.
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
		// Orphan locator pointing at an already-freed block: the block
		// bookkeeping for this hash is complete (the sweep clears the marker).
		return true, 0, nil
	}

	// The GC sweep and reconcile serialize on the per-remote lock, so this count
	// cannot change under us: it reliably tells the last-chunk case (marker
	// cleared last, retryable) from a partial one (marker cleared first, gated).
	lastChunk := rec.LiveChunkCount <= 1

	if !lastChunk {
		// Partial: clear the marker BEFORE decrementing so a re-visit resolves
		// synced=false above and cannot double-decrement a live sibling's block.
		if derr := r.Locators.DeleteSynced(ctx, hash); derr != nil {
			return false, 0, fmt.Errorf("block reclaim: delete synced marker %s: %w", hash, derr)
		}
	}

	remaining, err := r.Records.DecrLiveChunkCount(ctx, blockID, 1)
	if err != nil {
		return false, 0, fmt.Errorf("block reclaim: decr live chunk count %s: %w", blockID, err)
	}

	if remaining > 0 {
		return true, 0, nil // block still has live chunks — keep it (marker cleared above)
	}

	// Last live chunk gone: free the remote object, then the record, then clear
	// the marker. A DeleteBlock failure returns here with the marker still set,
	// so the next sweep retries; the record + object are retained for it (and for
	// the reconcile class-1 zero-ref backstop) — never dropped on a failed delete.
	if derr := r.RemoteBlocks.DeleteBlock(ctx, blockID); derr != nil {
		return false, 0, fmt.Errorf("block reclaim: delete block %s: %w", blockID, derr)
	}
	if derr := r.Records.DeleteBlockRecord(ctx, blockID); derr != nil {
		return false, 0, fmt.Errorf("block reclaim: delete record %s: %w", blockID, derr)
	}
	if derr := r.Locators.DeleteSynced(ctx, hash); derr != nil {
		return false, 0, fmt.Errorf("block reclaim: delete synced marker %s: %w", hash, derr)
	}
	return true, rec.Length, nil
}

// ensure compile-time interface satisfaction.
var _ BlockReclaimer = (*BlockGCReclaimer)(nil)
