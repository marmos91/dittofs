package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ReadAt reads data from storage at the given offset into dest.
// a non-nil/non-empty []BlockRef carries the CAS hashes covering the
// requested range (zero-filling sparse holes).
//
// After a successful read the engine calls cache.OnRead(payloadID
// blockHashes, fileSize) so the Cache's sequential-detection state
// machine can fire prefetch on upcoming hashes. The cache is hint-only
// here; reads always go through local/remote stores.
func (bs *Store) ReadAt(ctx context.Context, payloadID string, blocks []block.BlockRef, data []byte, offset uint64) (int, error) {
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
		bs.loadCache().OnRead(payloadID, hashes, computeFileSize(blocks))
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
// local-store rollup layer (pkg/block/local/fs/rollup.go
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
func (bs *Store) WriteAt(ctx context.Context, payloadID string, currentBlocks []block.BlockRef, data []byte, offset uint64) ([]block.BlockRef, error) {
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
	bs.loadCache().OnRead(payloadID, nil, 0)
	// the FastCDC chunker output is
	// produced by the local-store rollup pump
	// (pkg/block/local/fs/rollup.go:rollupFile) and lands as
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
func (bs *Store) Truncate(ctx context.Context, payloadID string, currentBlocks []block.BlockRef, newSize uint64) ([]block.BlockRef, error) {
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
	var kept []block.BlockRef
	if len(currentBlocks) > 0 {
		kept = make([]block.BlockRef, 0, len(currentBlocks))
		var dropped []block.BlockRef
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

		// Reap each dropped tail block's OWN row (by exact ID
		// "{payloadID}/{offset}") so its hash leaves EnumerateFileBlocks once
		// no row anywhere references it and the GC sweep can reclaim the remote
		// chunk (otherwise truncated tail chunks leak on the remote forever —
		// #832).
		//
		// By-ID, not by-hash: each {payloadID}/offset row is independent and
		// unique per offset. Dropped blocks are strictly past newSize, so their
		// offsets never collide with KEPT blocks' offsets — removing a dropped
		// block's row can never touch a kept block's row. Dedupe by OFFSET (a
		// defensive guard against a malformed duplicate-offset block list); two
		// different offsets carrying the SAME content hash are TWO rows and BOTH
		// must be reaped. Cross-file dedup keep-alive is provided by sibling
		// rows in other files keeping the hash in the GC live set — GC sweeps
		// the chunk only when no row anywhere references it — so reaping this
		// file's own rows by ID strands nothing another file still references.
		if bs.coordinator != nil {
			reaped := make(map[uint64]struct{}, len(dropped))
			for _, b := range dropped {
				if _, done := reaped[b.Offset]; done {
					continue
				}
				reaped[b.Offset] = struct{}{}
				if _, err := bs.coordinator.DecrementRefCountAndReap(ctx, payloadID, b.Offset); err != nil {
					return currentBlocks, fmt.Errorf("reap block on truncate-drop %s/%d: %w", payloadID, b.Offset, err)
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
	bs.loadCache().OnRead(payloadID, nil, 0)

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

// PunchHole implements the block-store side of NFSv4.2 DEALLOCATE (RFC 7862):
// it makes [offset, offset+length) of payloadID read back as zeros and reclaims
// the storage of any whole CAS block that falls entirely inside the range.
//
// currentBlocks is the file's pre-op block list. Blocks fully inside the
// punched range are dropped and their dedup refcounts decremented (so the GC
// sweep can reclaim the remote chunk, mirroring Truncate's by-ID reap); a block
// only partially overlapping the range is KEPT, because its content hash still
// addresses the surviving bytes — the partially-overlapped bytes are zeroed by
// the overwrite below. The returned []BlockRef is the pruned list (whole-block
// drops removed) for the caller to persist; an empty input returns nil
// (dual-read shim semantics).
//
// To guarantee zeros on the read path regardless of rollup state, the range is
// overwritten with zeros via the local append log (the same hot path WriteAt
// uses). Zero chunks dedup to a single CAS object, so this does not defeat
// space reclaim in aggregate. length == 0, or a payload with no blocks, is a
// no-op success.
func (bs *Store) PunchHole(ctx context.Context, payloadID string, currentBlocks []block.BlockRef, offset, length uint64) ([]block.BlockRef, error) {
	if err := bs.enter(); err != nil {
		return currentBlocks, err
	}
	defer bs.closeMu.RUnlock()
	if length == 0 {
		return currentBlocks, nil
	}
	// Reject a range whose offset+length wraps uint64: a wrapped end would make
	// the reap predicate and the zero-overwrite loop operate on an unintended
	// (smaller) range, silently violating DEALLOCATE semantics. Adapters already
	// validate this, but guard at the store boundary too.
	if offset > ^uint64(0)-length {
		return currentBlocks, fmt.Errorf("punch range overflow: offset=%d length=%d", offset, length)
	}
	end := offset + length

	// Reap blocks lying ENTIRELY within [offset, end). Partially-overlapping
	// blocks are kept (their hash still addresses surviving bytes); the zero
	// overwrite below masks the punched portion on read.
	kept := currentBlocks
	if len(currentBlocks) > 0 {
		kept = make([]block.BlockRef, 0, len(currentBlocks))
		var dropped []block.BlockRef
		for _, b := range currentBlocks {
			bEnd := b.Offset + uint64(b.Size)
			if b.Offset >= offset && bEnd <= end {
				dropped = append(dropped, b)
				continue
			}
			kept = append(kept, b)
		}
		if bs.coordinator != nil {
			reaped := make(map[uint64]struct{}, len(dropped))
			for _, b := range dropped {
				if _, done := reaped[b.Offset]; done {
					continue
				}
				reaped[b.Offset] = struct{}{}
				if _, err := bs.coordinator.DecrementRefCountAndReap(ctx, payloadID, b.Offset); err != nil {
					return currentBlocks, fmt.Errorf("reap block on punch %s/%d: %w", payloadID, b.Offset, err)
				}
			}
		}
	}

	// Overwrite the range with zeros so both the pre-rollup append-log read path
	// and the post-rollup CAS path return zeros. Chunked to bound the transient
	// buffer for large deallocations.
	const zeroChunk = 1 << 20 // 1 MiB
	zeros := make([]byte, zeroChunk)
	for pos := offset; pos < end; {
		n := end - pos
		if n > zeroChunk {
			n = zeroChunk
		}
		if err := bs.local.AppendWrite(ctx, payloadID, zeros[:n], pos); err != nil {
			return currentBlocks, fmt.Errorf("zero punched range %s [%d,%d): %w", payloadID, pos, pos+n, err)
		}
		pos += n
	}

	bs.loadCache().OnRead(payloadID, nil, 0)

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
func (bs *Store) Delete(ctx context.Context, payloadID string, blocks []block.BlockRef) error {
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
		bs.loadCache().InvalidateFile(payloadID, blockRefHashes(blocks))
	} else {
		// Legacy/dual-read empty-blocks path: at least reset the
		// per-payload tracker so prefetch doesn't chase stale hashes.
		bs.loadCache().OnRead(payloadID, nil, 0)
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
		// Reap each block's OWN row by exact ID "{payloadID}/{offset}". Each
		// {payloadID}/offset row is independent and unique per offset, so reap
		// each block once, deduped by OFFSET (a defensive guard against a
		// malformed duplicate-offset list). The SAME content hash at TWO
		// offsets in this file is TWO rows and BOTH must be reaped.
		//
		// By-ID, not by-hash: cross-file dedup keep-alive is provided by
		// SIBLING rows in other files keeping the hash in EnumerateFileBlocks
		// (the GC live set). GC sweeps the chunk only when no row anywhere
		// references the hash, so removing this file's own rows by ID strands
		// nothing another file still references.
		reaped := make(map[uint64]struct{}, len(blocks))
		for _, b := range blocks {
			if _, done := reaped[b.Offset]; done {
				continue
			}
			reaped[b.Offset] = struct{}{}
			// Reap at RefCount 0 so the row leaves EnumerateFileBlocks once no
			// sibling references the hash, letting the GC sweep reclaim the
			// remote chunk (#832).
			newCount, err := bs.coordinator.DecrementRefCountAndReap(ctx, payloadID, b.Offset)
			if err != nil {
				if coordErr == nil {
					coordErr = fmt.Errorf("reap block on delete %s/%d: %w", payloadID, b.Offset, err)
				}
				continue
			}
			// Row reaped (count hit zero): the local CAS chunk for this hash
			// may be reclaimed, so drop the synced marker too. Without this
			// cascade the synced set would drift out of strict-subset
			// relationship with local CAS contents — a future re-Put of the
			// same hash would skip remote upload because the marker is stale.
			// Failure here is benign (the marker becomes an orphan, but a stale
			// marker only causes a single skipped upload on a re-Put; the bytes
			// are already remote-resident from the original Mark). A sibling
			// file still referencing this hash would likewise re-upload on its
			// next flush — also benign. Logged at Warn for operator visibility.
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

// CopyPayload duplicates a file's content by referencing the source's
// content-addressed blocks — no data movement, O(blocks) metadata puts.
// It does two things for the destination:
//
//  1. Creates one per-(dstPayloadID/offset) FileBlock row for every source
//     block, keyed by the SAME offset + hash + DataSize, in BlockStatePending.
//     This is the load-bearing step for read correctness: the cold-read path
//     resolves a payload's bytes via ListFileBlocks(dstPayloadID) (the per-file
//     FileBlock rows), NOT via FileAttr.Blocks. Without dst rows a read of the
//     clone hits the sparse-block branch and zero-fills — silent corruption
//     (#1384). Because every row carries the source hash and the chunks are
//     content-addressed, the dst rows resolve to the SAME shared CAS chunks and
//     the clone reads back byte-identical to the source.
//  2. Bumps each unique source-hash RefCount via the coordinator. This is now
//     belt-and-suspenders: block keep-alive (and GC safety) is by-hash over the
//     manifest live set in the GC mark phase, which enumerates the dst rows too.
//
// It returns a deep copy of srcBlocks as the destination's BlockRef list, which
// the caller persists as dst's FileAttr.Blocks in the same metadata txn — so the
// per-file rows and the FileAttr.Blocks manifest reference the same hashes and
// offsets.
//
// Empty srcBlocks => nil-safe legacy path: copies nothing (legacy
// CopyPayload data-copy semantics are removed in; the legacy
// adapter call sites that need data copies should drive ReadAt+WriteAt
// directly during the dual-read window). Production callers always
// supply a snapshot of the source file's FileAttr.Blocks.
//
// Failure semantics: a genuine IncrementRefCount backend fault is surfaced
// immediately without further increments (the caller's metadata txn rolls back).
// A missing-row increment (ErrFileBlockNotFound) is tolerated — see the loop.
//
// Dedup: a single hash present multiple times in srcBlocks bumps the
// RefCount only once per CopyPayload call (per-call seen-hash set).
// The destination's []BlockRef preserves the original sequence so
// subsequent reads still resolve every offset correctly. Dst rows are
// created per source block (one row per offset) — two BlockRefs sharing a
// hash are two distinct dst rows, mirroring the per-offset row model used by
// the rollup ObjectIDPersister and the Delete/Truncate by-ID reap contract.
func (bs *Store) CopyPayload(ctx context.Context, srcPayloadID, dstPayloadID string, srcBlocks []block.BlockRef) ([]block.BlockRef, error) {
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
	seen := make(map[block.ContentHash]struct{}, len(srcBlocks))
	for _, b := range srcBlocks {
		if _, ok := seen[b.Hash]; ok {
			continue
		}
		seen[b.Hash] = struct{}{}
		if err := bs.coordinator.IncrementRefCount(ctx, b.Hash); err != nil {
			// A CAS block is content-addressed and lives in BlockStatePending
			// for the life of the payload (it never transitions to Remote), so
			// the Remote-gated GetByHash that IncrementRefCount relies on
			// resolves it to "no FileBlock row" and returns ErrFileBlockNotFound.
			// That is NOT a clone failure: the destination's BlockRef manifest
			// (dst, below) references the same hashes, and block keep-alive is
			// by-hash over the manifest live set in the GC mark phase
			// (EnumerateFileBlocks / reapSupersededFileBlocks), not via RefCount.
			// So a missing-row increment is a no-op to be skipped, mirroring how
			// DecrementRefCount already tolerates the same miss. Without this,
			// NFSv4.2 CLONE and SMB server-side-copy fail with EREMOTEIO on any
			// rolled-up source (#1384). A genuine backend fault still aborts.
			if errors.Is(err, block.ErrFileBlockNotFound) {
				continue
			}
			return nil, fmt.Errorf("CopyPayload: increment refcount on %s: %w", b.Hash.String(), err)
		}
	}

	// Create the destination's per-file FileBlock rows. The cold-read path
	// resolves bytes via ListFileBlocks(dstPayloadID), not FileAttr.Blocks, so
	// without these rows reads of the clone zero-fill (silent corruption,
	// #1384). One row per source block keyed by dstPayloadID + the SAME offset,
	// hash and DataSize, in BlockStatePending — mirroring the rollup
	// ObjectIDPersister (engine.go) so the dst rows resolve to the shared CAS
	// chunks (content-addressed by hash). Skip zero-hash blocks (sparse holes
	// carry no chunk). No data is moved; this is O(blocks) metadata puts.
	//
	// Route the Put through the txn bound in ctx when present. The clone
	// callers (common.CopyPayload / common.CloneWholeFile) invoke us inside
	// metadataStore.WithTransaction, which on the memory backend holds the
	// store mutex for the life of fn; the store-level fileBlockStore.Put would
	// re-acquire that same (non-reentrant) mutex and self-deadlock. The
	// tx-bound Put writes under the already-held lock and commits/rolls back
	// atomically with the caller's dst FileAttr.Blocks PutFile — so the per-file
	// rows and the FileAttr.Blocks manifest stay consistent. With no bound txn
	// (e.g. unit tests wiring the engine directly) fall back to the store-level
	// Put, matching the rollup ObjectIDPersister which runs outside any txn.
	// Persist dst rows through the txn bound in ctx when present, else the
	// store directly. The bound txn writes under the lock WithTransaction
	// already holds — a separate store-level Put would re-enter the memory
	// backend's non-reentrant mutex and self-deadlock. The store fallback
	// covers the no-txn path (unit tests wiring the engine directly), matching
	// the rollup ObjectIDPersister. The row write does NOT depend on
	// bs.fileBlockStore being non-nil when a txn is bound.
	tx := metadata.TxFromContext(ctx)
	var putRow func(context.Context, *block.FileBlock) error
	switch {
	case tx != nil:
		putRow = tx.Put
	case bs.fileBlockStore != nil:
		putRow = bs.fileBlockStore.Put
	}
	for _, b := range srcBlocks {
		if b.Hash.IsZero() {
			continue
		}
		// Refuse to produce an unreadable clone: if there are blocks to
		// materialize but no way to persist the dst rows, fail loudly rather
		// than copy only the manifest (whose blocks the cold-read path can't
		// resolve without rows → zero-fill, #1384).
		if putRow == nil {
			return nil, fmt.Errorf("CopyPayload: no transaction or file-block store to persist dst rows for %s", dstPayloadID)
		}
		fb := &block.FileBlock{
			ID:       fmt.Sprintf("%s/%d", dstPayloadID, b.Offset),
			Hash:     b.Hash,
			DataSize: b.Size,
			State:    block.BlockStatePending,
		}
		if err := putRow(ctx, fb); err != nil {
			return nil, fmt.Errorf("CopyPayload: FileBlock.Put(%s): %w", fb.ID, err)
		}
	}

	// Deep-copy the slice (BlockRef is a value type — append over nil
	// produces a fresh backing array independent of srcBlocks). The caller
	// persists this as dst's FileAttr.Blocks in the same metadata txn, so the
	// rows above and this manifest stay consistent (same hashes + offsets).
	dst := append([]block.BlockRef(nil), srcBlocks...)

	// srcPayloadID is retained in the signature for future use (cache prefetch
	// hints, identity-based dedup) and to match the public Writer interface.
	_ = srcPayloadID

	return dst, nil
}

// reapSupersededFileBlocks deletes the per-file FileBlock rows that a
// rollup pass's re-chunk superseded (#953). A row is superseded when its
// chunk offset falls inside the byte region this pass rewrote
// (newBlocks span) but is NOT one of the new chunk offsets — i.e. an
// old-generation chunk that an in-place overwrite re-chunked onto
// different FastCDC boundaries. Left behind, such rows accumulate in the
// CAS manifest (ListFileBlocks) and the cold read path mixes generations,
// returning stale bytes after log compaction or local-state eviction.
//
// Region-scoped + by-exact-ID, mirroring the engine Delete/Truncate reap
// contract:
//   - Only rows whose offset is in [regionStart, regionEnd) are eligible —
//     a row strictly before the region (a straddling predecessor the FS
//     rollup could not boundary-align because its chunk was not locally
//     readable) is KEPT so its non-overwritten head still serves bytes.
//   - Rows whose offset is reused by a new chunk (overwritten in place by
//     FileBlock.Put above) are skipped — they are the current generation.
//   - Reap by EXACT ID "{payloadID}/{offset}" via DecrementRefCountAndReap.
//     Each {payloadID}/offset row is independent; reaping this file's own
//     row never touches another file's row. Cross-file dedup keep-alive is
//     by-hash in the GC live set (EnumerateFileBlocks): the chunk is
//     reclaimed only when no row anywhere references the hash, so a hash a
//     sibling file still uses stays alive even after this row is reaped.
//
// No-ops when the coordinator is unwired or when priorOffsets is empty
// (first write). When newBlocks is empty all prior rows are reaped
// unconditionally (file truncated to zero bytes this pass).
func (bs *Store) reapSupersededFileBlocks(ctx context.Context, payloadID string, priorOffsets []uint64, newBlocks []block.BlockRef) error {
	if bs.coordinator == nil || len(priorOffsets) == 0 {
		return nil
	}

	// superseded decides whether a prior offset is reaped this pass.
	//
	// When newBlocks is empty no chunks were produced (file truncated to zero
	// bytes, or the reconstructed stream was entirely clipped by the
	// truncation fence): every prior row is unconditionally superseded.
	// Otherwise a prior offset is superseded only when it falls inside the
	// rewritten region [regionStart, regionEnd) and is not itself a reused
	// offset (those rows were overwritten in place by FileBlock.Put and are
	// the current generation).
	var superseded func(off uint64) bool
	if len(newBlocks) == 0 {
		superseded = func(uint64) bool { return true }
	} else {
		regionStart := newBlocks[0].Offset
		var regionEnd uint64
		newOffsets := make(map[uint64]struct{}, len(newBlocks))
		for _, b := range newBlocks {
			if b.Offset < regionStart {
				regionStart = b.Offset
			}
			if end := b.Offset + uint64(b.Size); end > regionEnd {
				regionEnd = end
			}
			newOffsets[b.Offset] = struct{}{}
		}
		superseded = func(off uint64) bool {
			if off < regionStart || off >= regionEnd {
				return false // outside the rewritten region — untouched, keep.
			}
			_, isNew := newOffsets[off]
			return !isNew // reused offset is the current generation — keep.
		}
	}

	reaped := make(map[uint64]struct{}, len(priorOffsets))
	for _, off := range priorOffsets {
		if !superseded(off) {
			continue
		}
		if _, done := reaped[off]; done {
			continue
		}
		reaped[off] = struct{}{}
		if _, err := bs.coordinator.DecrementRefCountAndReap(ctx, payloadID, off); err != nil {
			return fmt.Errorf("reap superseded block %s/%d: %w", payloadID, off, err)
		}
	}
	return nil
}
