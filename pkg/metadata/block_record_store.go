package metadata

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/marmos91/dittofs/pkg/block"
)

// ManifestToChunkRefs projects FileChunk manifest rows into the canonical
// offset-sorted ChunkRef list. Rows with an unparseable ID are skipped. The
// per-file FileChunk manifest is the switchover's single source of truth;
// File.Blocks is a materialized projection of it, kept coherent-by-construction.
func ManifestToChunkRefs(rows []*block.FileChunk) []block.ChunkRef {
	refs := make([]block.ChunkRef, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		off, ok := block.ParseChunkOffset(r.ID)
		if !ok {
			continue
		}
		refs = append(refs, block.ChunkRef{Hash: r.Hash, Offset: off, Size: r.DataSize})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Offset < refs[j].Offset })
	if len(refs) == 0 {
		return nil
	}
	return refs
}

// mergeCommittedRefs folds just-written manifest rows into refs — the
// offset-sorted projection of the manifest as it stood before those rows — and
// returns what ManifestToChunkRefs would produce over the manifest they landed
// in. A manifest holds exactly one row per offset, so a row whose offset refs
// already carries replaces that entry and every other row lands at its sorted
// position. Rows are applied in slice order, so a batch repeating an offset
// keeps the last one — the single row the store retains. Rows with an
// unparseable ID, or addressing another payload, are skipped, matching what the
// full projection's list-and-sort ignores.
// It also returns the offsets it touched — always non-nil, so callers can hand
// the store the exact set that may differ from the rows it already holds.
func mergeCommittedRefs(refs []block.ChunkRef, payloadID string, rows []*block.FileChunk) ([]block.ChunkRef, []uint64) {
	byOffset := make(map[uint64]block.ChunkRef, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		off, ok := block.ParseChunkOffset(r.ID)
		if !ok || chunkPayloadID(r.ID) != payloadID {
			continue
		}
		byOffset[off] = block.ChunkRef{Hash: r.Hash, Offset: off, Size: r.DataSize}
	}
	added := make([]block.ChunkRef, 0, len(byOffset))
	changed := make([]uint64, 0, len(byOffset))
	for off, ref := range byOffset {
		added = append(added, ref)
		changed = append(changed, off)
	}
	if len(added) == 0 {
		return refs, changed
	}
	// Offsets are unique here, so this total order is the one the full
	// projection's sort produces.
	sort.Slice(added, func(i, j int) bool { return added[i].Offset < added[j].Offset })

	merged := make([]block.ChunkRef, 0, len(refs)+len(added))
	i, j := 0, 0
	for i < len(refs) && j < len(added) {
		switch {
		case refs[i].Offset < added[j].Offset:
			merged = append(merged, refs[i])
			i++
		case added[j].Offset < refs[i].Offset:
			merged = append(merged, added[j])
			j++
		default: // this batch rewrote the offset — its row wins
			merged = append(merged, added[j])
			i++
			j++
		}
	}
	return append(append(merged, refs[i:]...), added[j:]...), changed
}

// ProjectCommittedChunks folds the manifest rows a caller just wrote into
// File.Blocks, within that same txn, without re-listing the manifest: Blocks
// already holds the projection of every earlier row, so merging the new rows in
// at their sorted position yields exactly what ProjectManifestToBlocks
// re-derives, at the cost of the batch rather than of the whole file — carving
// one file into many block objects would otherwise re-list and re-sort the whole
// growing manifest once per object.
//
// rows must be exactly the rows just written for payloadID in this txn. An empty
// Blocks list carries no projection to merge into, so that case re-derives from
// the manifest — which is also what makes the first commit of a file correct.
func ProjectCommittedChunks(ctx context.Context, tx Transaction, payloadID string, rows []*block.FileChunk) error {
	if payloadID == "" {
		return nil
	}
	file, err := fileToProject(ctx, tx, payloadID)
	if err != nil || file == nil {
		return err
	}
	if len(file.Blocks) == 0 {
		return reprojectFile(ctx, tx, file, payloadID)
	}
	merged, changed := mergeCommittedRefs(file.Blocks, payloadID, rows)
	return putProjection(ctx, tx, file, merged, changed)
}

// ProjectManifestToBlocks re-materializes File.Blocks for payloadID from the
// current FileChunk manifest, within the caller's txn. Every manifest mutation
// (carve commit, reap + re-carve straddle, truncate) must project in the
// SAME txn that changed the rows, so File.Blocks == projection(rows) always and
// the raw-row readers (snapshot WriteSnapshot, refcount audit) never see drift.
// A caller that knows which rows it changed holds that invariant far more
// cheaply through ProjectCommittedChunks; this full re-derivation is for the
// ones that don't (a reap, or rows of unknown provenance).
// A missing file (deleted concurrently) is a no-op. ponytail: this is the
// switchover bridge; the #1715 fb-split removes File.Blocks from the row entirely
// and derives at read time, retiring this projection.
func ProjectManifestToBlocks(ctx context.Context, tx Transaction, payloadID string) error {
	if payloadID == "" {
		return nil
	}
	file, err := fileToProject(ctx, tx, payloadID)
	if err != nil || file == nil {
		return err
	}
	return reprojectFile(ctx, tx, file, payloadID)
}

// fileToProject resolves the File row the projection writes onto. A missing row
// (block-layer fixtures with synthetic payloadIDs, or a file deleted between
// carve and commit) yields (nil, nil) — nothing to project onto.
func fileToProject(ctx context.Context, tx Transaction, payloadID string) (*File, error) {
	file, err := tx.GetFileByPayloadID(ctx, PayloadID(payloadID))
	if err != nil {
		if IsNotFoundError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("project blocks: get file for %s: %w", payloadID, err)
	}
	return file, nil
}

func reprojectFile(ctx context.Context, tx Transaction, file *File, payloadID string) error {
	rows, err := tx.ListFileChunks(ctx, payloadID)
	if err != nil {
		return fmt.Errorf("project blocks: list manifest for %s: %w", payloadID, err)
	}
	// A full re-derivation says nothing about which offsets moved, so the
	// store gets no scope and diffs everything.
	return putProjection(ctx, tx, file, ManifestToChunkRefs(rows), nil)
}

// putProjection persists refs as the file's manifest projection. changed, when
// non-nil, is the only set of offsets that can differ from what the store
// already holds; nil leaves the store to work that out for itself.
func putProjection(ctx context.Context, tx Transaction, file *File, refs []block.ChunkRef, changed []uint64) error {
	file.Blocks = refs
	// A projection from the manifest IS a manifest write — persist it. This
	// funnels carve/rollup commit (DefaultCommitBlock), the reap+re-carve
	// (ReapSupersededManifest), and coordinator.ReprojectBlocks.
	file.BlocksDirty = true
	file.BlocksDirtyOffsets = changed
	err := tx.PutFile(ctx, file)
	// The scope describes this write only — clearing it stops a caller that
	// reuses the struct for a wider manifest mutation from inheriting a promise
	// that no longer holds.
	file.BlocksDirtyOffsets = nil
	return err
}

// chunkPayloadID returns the payload prefix of a manifest row ID — everything
// before the last '/' — or "" when there is no prefix to take. The offset that
// follows is not validated here, so an ID carrying an empty or non-numeric
// offset still yields its prefix: callers parse the offset themselves and drop
// that row, whereas answering "" would skip the projection of the whole batch
// the row arrived in rather than the one bad row.
func chunkPayloadID(id string) string {
	if i := strings.LastIndexByte(id, '/'); i > 0 {
		return id[:i]
	}
	return ""
}

// payloadIDFromChunks extracts the shared payloadID from a carve pass's FileChunk
// rows (all rows of one carve belong to one file). Returns "" when the rows are
// nil/empty or none of them carries a payload prefix, which callers treat as
// "skip projection".
func payloadIDFromChunks(fileChunks []*block.FileChunk) string {
	for _, fc := range fileChunks {
		if fc == nil {
			continue
		}
		if pid := chunkPayloadID(fc.ID); pid != "" {
			return pid
		}
	}
	return ""
}

// BlockRecordStore manages the lifecycle of log-blob block records.
// Each record tracks the sync state, live chunk count, and hash of a
// single block object. Implementations MUST be safe for concurrent use.
type BlockRecordStore interface {
	// PutBlockRecord writes or overwrites the block record for rec.BlockID.
	PutBlockRecord(ctx context.Context, rec block.BlockRecord) error

	// GetBlockRecord retrieves the block record for blockID.
	// Returns (_, false, nil) when no record exists — absence is not an error.
	GetBlockRecord(ctx context.Context, blockID string) (block.BlockRecord, bool, error)

	// DeleteBlockRecord removes the block record for blockID.
	// Idempotent: deleting an absent record returns nil.
	DeleteBlockRecord(ctx context.Context, blockID string) error

	// WalkBlockRecords calls fn for every stored block record in
	// implementation-defined order. Returns the first non-nil error from fn
	// or from the underlying store iterator.
	WalkBlockRecords(ctx context.Context, fn func(block.BlockRecord) error) error

	// DecrLiveChunkCount atomically decrements the LiveChunkCount for blockID
	// by delta, flooring at 0. Returns the remaining count after the decrement.
	// Returns an error if blockID does not exist.
	DecrLiveChunkCount(ctx context.Context, blockID string, delta uint32) (remaining uint32, err error)
}

// DefaultCommitBlock atomically writes a block record and every chunk's synced
// marker + remote locator within a SINGLE transaction. Either the whole commit
// is visible or none of it is — there is no partially-committed state to retry,
// so a commit error simply propagates to the caller (whose existing requeue
// logic re-drives the batch).
//
// Semantics:
//
//   - Idempotent on BlockID: if the block record already exists the function
//     is a no-op (LiveChunkCount is not double-counted, locators untouched).
//   - Locator writes are LAST-WINS: PutSyncedLocators inside the tx overwrites
//     any existing locator with the new block locator. The direct MarkSynced
//     method stays first-wins; CommitBlock needs overwrite because the
//     cas→blocks migration re-commits chunks whose standalone (zero-BlockID)
//     locators must be rewritten to point into the new block.
//
// Exported so Store implementations in sub-packages can delegate CommitBlock
// to this shared logic.
func DefaultCommitBlock(
	ctx context.Context,
	s Transactor,
	rec block.BlockRecord,
	chunks []block.BlockChunkCommit,
	fileChunks []*block.FileChunk,
) error {
	return s.WithTransaction(ctx, func(tx Transaction) error {
		_, exists, err := tx.GetBlockRecord(ctx, rec.BlockID)
		if err != nil {
			return err
		}
		if exists {
			return nil // idempotent: already committed
		}
		if err := tx.PutBlockRecord(ctx, rec); err != nil {
			return err
		}
		// Per-file manifest rows: the block carver passes one FileChunk per chunk
		// (ID={FileID}/{FileOffset}, Hash, DataSize, State=Pending); legacy callers
		// pass nil and write no rows.
		for _, fc := range fileChunks {
			if fc == nil {
				continue
			}
			if err := tx.Put(ctx, fc); err != nil {
				return err
			}
		}
		// Locator overwrite (last-wins), see the function comment. MarkSynced
		// alone would be first-wins and leave a stale standalone locator in
		// place; the batched write applies the overwrite for the whole commit
		// at once rather than two calls per chunk.
		if len(chunks) > 0 {
			if err := tx.PutSyncedLocators(ctx, chunks); err != nil {
				return err
			}
		}
		// Materialize File.Blocks from the manifest in this same txn so raw-row
		// readers (snapshot, audit) stay coherent — merging only this batch's
		// rows, since re-deriving from the whole manifest costs a full list and
		// sort per committed block object. Skipped for legacy callers that pass
		// no fileChunks (empty payloadID). Superseded-row reaping happens once
		// per carve run (ReapSupersededManifest), not per batch — see below.
		return ProjectCommittedChunks(ctx, tx, payloadIDFromChunks(fileChunks), fileChunks)
	})
}

// ReapSupersededManifest deletes the manifest rows a carve run supersedes and
// re-projects File.Blocks, atomically. A partial overwrite re-chunks the dirty
// range (plus its warm straddle remainders, re-marked dirty by the journal) into
// fresh rows; the old rows those supersede must be reaped or the per-file manifest
// no longer tiles [0,size) — a cold read then resolves a stale straddling row
// (returns old bytes) or hits a gap (zero-fills). That is #953.
//
// Reaped set: every existing row for payloadID whose start offset lies in the
// run's [runStart, runEnd) span and is NOT one of the offsets this run just wrote
// (newOffsets). Running once at run end — after all of the run's batches have
// committed their rows — is what makes it correct across a multi-batch run: a
// straddler spanning a batch seam has no single batch span that contains it, and
// reaping per batch by the run span would delete a sibling batch's fresh rows.
// The run span covers only re-carved (dirty) bytes, so an un-recarved cold
// remainder falls outside it and is never reaped — no gap. newOffsets excludes
// this run's own rows so they survive.
//
// ponytail: this fixes read-coherence — the corruption. Decrementing the reaped
// chunk's CAS refcount to reclaim its remote space is a separate, tracked
// follow-up (#1715): under-counting only leaks space, it never drops live data.
func ReapSupersededManifest(ctx context.Context, tx Transaction, payloadID string, runStart, runEnd int64, newOffsets map[int64]struct{}) error {
	if payloadID == "" || runEnd <= runStart {
		return nil
	}
	rows, err := tx.ListFileChunks(ctx, payloadID)
	if err != nil {
		return fmt.Errorf("reap superseded: list manifest for %s: %w", payloadID, err)
	}
	for _, r := range rows {
		if r == nil {
			continue
		}
		off, ok := block.ParseChunkOffset(r.ID)
		if !ok {
			continue
		}
		if int64(off) < runStart || int64(off) >= runEnd {
			continue // outside the re-carved run — untouched (incl. cold remainders)
		}
		if _, isNew := newOffsets[int64(off)]; isNew {
			continue // a row this run just wrote — keep it
		}
		if err := tx.Delete(ctx, r.ID); err != nil {
			return fmt.Errorf("reap superseded: delete %s: %w", r.ID, err)
		}
	}
	return ProjectManifestToBlocks(ctx, tx, payloadID)
}
