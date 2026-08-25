package metadata

import (
	"context"
	"errors"
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
	file.ManifestDirtyOffsets = changed
	err := tx.SetManifest(ctx, file)
	// The scope describes this write only — clearing it stops a caller that
	// reuses the struct for a wider manifest mutation from inheriting a promise
	// that no longer holds.
	file.ManifestDirtyOffsets = nil
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
		// per carve pass (ReapSupersededManifest), not per batch — see below.
		return ProjectCommittedChunks(ctx, tx, payloadIDFromChunks(fileChunks), fileChunks)
	})
}

// ManifestRowEndAfter reports how far the manifest coverage straddling off
// reaches: the greatest end among the rows that start strictly before off and
// extend past it, or off when none does. Rows starting in the range that opens up
// are folded in too, so the answer is a point past which no straddling row is
// left — with overlapping rows (a truncate-narrow plus a re-carve leaves some) a
// single pass would stop one row short.
//
// A carve run asks this about its own end. A run that stops inside a row leaves
// that row half superseded, and the reap spares it whole rather than strand the
// part past the run — a row claims a prefix of its chunk, so no row can be made
// to start mid-chunk and cover that part instead. The stale row then overlaps
// the fresh tiling for good. Carving through to this offset is what lets the
// reap delete it.
//
// A payload with no rows yet (the first carve of a file) is not an error: there
// is nothing to straddle, so the run stands as snapshotted.
//
// ponytail: O(n log n) over the whole manifest per carve run, on top of the reap's
// own scan; a straddle index would pay off only once profiling at real chunk
// counts says these two scans matter.
func ManifestRowEndAfter(ctx context.Context, tx Transaction, payloadID string, off int64) (int64, error) {
	if payloadID == "" {
		return off, nil
	}
	rows, err := tx.ListFileChunks(ctx, payloadID)
	if err != nil {
		if errors.Is(err, block.ErrFileChunkNotFound) {
			return off, nil
		}
		return 0, fmt.Errorf("manifest row end after %d for %s: %w", off, payloadID, err)
	}
	spans := make([][2]int64, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		rowOff, ok := block.ParseChunkOffset(r.ID)
		if !ok {
			continue
		}
		spans = append(spans, [2]int64{int64(rowOff), int64(rowOff) + int64(r.DataSize)})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	end := off
	for _, sp := range spans {
		if sp[0] >= end {
			break // rows are sorted by start: nothing later can straddle end either
		}
		if sp[1] > end {
			end = sp[1]
		}
	}
	return end, nil
}

// ReapSupersededManifest deletes the manifest rows a carve pass supersedes and
// re-projects File.Blocks, atomically. A partial overwrite re-chunks the dirty
// range (plus its warm straddle remainders, re-marked dirty by the journal) into
// fresh rows; the old rows those supersede must be reaped or the per-file manifest
// no longer tiles [0,size) — a cold read then resolves a stale straddling row
// (returns old bytes) or hits a gap (zero-fills). That is #953.
//
// spans are the committed prefixes of the pass's dirty runs, as [start, end)
// pairs; they are disjoint, and the holes between them are un-recarved bytes no
// span may touch. newOffsets holds the offsets of every chunk the pass wrote, so
// its own fresh rows survive. Reaped set: every existing row that lies wholly
// inside a span and is not one of those offsets.
//
// One call per pass, not per run: each span's deletes are decided against the
// same listing, and File.Blocks is re-projected once at the end. That is what
// keeps the cost of a file with tens of thousands of dirty runs at one manifest
// read rather than one per run, all of it under the journal's carve lock. It is
// also equivalent to reaping the spans one at a time in ascending order, because
// a row this loop deletes or narrows ends at or before the span it acted on and
// so cannot reach the spans after it.
//
// Running after all of the pass's rows are committed is what makes it correct
// across a multi-block run: a straddler spanning a block seam has no single
// block span that contains it, and reaping per block by the run span would
// delete a sibling block's fresh rows.
//
// A row that STARTS before its span and reaches into it is not in the reaped set —
// deleting it would strip [rowStart, spanStart) of its only cover — so it is
// narrowed to the prefix it still owns instead: DataSize becomes spanStart-rowStart
// and the fresh rows take over from there. The chunk keeps every byte on the
// remote and is still hash-verified over all of them; the row just stops claiming
// the part the run re-chunked, which is the same shape a truncate's narrow leaves
// behind.
//
// A row that reaches past its span's end stays whole, whichever side it starts
// on: no row can start mid-chunk, so its tail past the span has no other cover,
// and deleting it would trade an overlap for a gap. Where it starts decides what
// that overlap then reads, and the two sides are not alike.
//
// Starting BEFORE the span is safe. Coverage resolves an overlap to the greatest
// covering start, and every fresh row inside the span starts later than a row
// that began before it, so the fresh rows win across the whole span and the
// survivor serves only its un-recarved tail.
//
// Starting INSIDE the span is not. Over [rowStart, spanEnd) the spared row is
// itself the greatest start, so it outranks the fresh row covering those bytes
// and serves the content they held before the carve. Sparing it is the least bad
// of three wrong answers, not a safe one: deleting it strands [spanEnd, rowEnd)
// with no cover at all, and it cannot be narrowed off the overlap either, because
// a row claims a prefix of its chunk and none can be made to start at spanEnd.
//
// Nothing here can do better, because the overlap is already decided by the time
// the reap runs: the fresh rows are committed, and no row that claims a prefix
// can cover [spanEnd, rowEnd) in the spared row's place. Only carving through to
// rowEnd avoids it, which is what the journal's run extension arranges whenever
// the bytes past the span are still warm; a span cuts a row starting inside it
// only where that extension could not reach — the tail is cold, evicted, holed,
// or already dirty for a later run.
//
// ponytail: over [rowStart, spanEnd) a cold read serves pre-carve bytes, and the
// manifest records no order to tell the caller so; closing it means carve
// hydrating the straddler's tail and re-chunking it, or refusing to commit a
// tiling whose span cuts a row starting inside it (#2124).
//
// ponytail: this fixes read-coherence — the corruption. Decrementing the reaped
// chunk's CAS refcount to reclaim its remote space is a separate, tracked
// follow-up (#1715): under-counting only leaks space, it never drops live data.
func ReapSupersededManifest(ctx context.Context, tx Transaction, payloadID string, spans [][2]int64, newOffsets map[int64]struct{}) error {
	if payloadID == "" {
		return nil
	}
	ordered := make([][2]int64, 0, len(spans))
	for _, sp := range spans {
		if sp[1] > sp[0] {
			ordered = append(ordered, sp)
		}
	}
	if len(ordered) == 0 {
		return nil
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i][0] < ordered[j][0] })

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
		rowStart := int64(off)
		rowEnd := rowStart + int64(r.DataSize)
		// The span this row is acted on: the first one whose end the row does not
		// reach past. An earlier span the row overlaps is one it outreaches, and
		// outreaching a span is what spares the row — acting on it there would
		// strand the stretch beyond, since no row can start mid-chunk to cover it.
		// A later span the row cannot reach, because acting here leaves it ending
		// at or before this span's start. Spans ascend, so this is a binary search.
		i := sort.Search(len(ordered), func(j int) bool { return ordered[j][1] >= rowEnd })
		if i == len(ordered) || ordered[i][0] >= rowEnd || ordered[i][1] <= rowStart {
			continue // no span acts on it: untouched (incl. cold remainders)
		}
		spanStart := ordered[i][0]
		if rowStart < spanStart {
			narrowed := *r
			narrowed.DataSize = uint32(spanStart - rowStart)
			if err := tx.Put(ctx, &narrowed); err != nil {
				return fmt.Errorf("reap superseded: narrow %s: %w", r.ID, err)
			}
			continue
		}
		if _, isNew := newOffsets[rowStart]; isNew {
			continue // a row this pass just wrote — keep it
		}
		if err := tx.Delete(ctx, r.ID); err != nil {
			return fmt.Errorf("reap superseded: delete %s: %w", r.ID, err)
		}
	}
	return ProjectManifestToBlocks(ctx, tx, payloadID)
}
