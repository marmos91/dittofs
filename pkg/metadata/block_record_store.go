package metadata

import (
	"context"
	"errors"
	"fmt"
	"math"
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
		refs = append(refs, block.ChunkRef{Hash: r.Hash, Offset: off, Size: r.DataSize, StartOffset: r.StartOffset})
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
		byOffset[off] = block.ChunkRef{Hash: r.Hash, Offset: off, Size: r.DataSize, StartOffset: r.StartOffset}
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
		// Locator overwrite (last-wins), see the function comment. MarkSynced
		// alone would be first-wins and leave a stale standalone locator in
		// place; the batched write applies the overwrite for the whole commit
		// at once rather than two calls per chunk.
		if len(chunks) > 0 {
			if err := tx.PutSyncedLocators(ctx, chunks); err != nil {
				return err
			}
		}
		// Per-file manifest rows: the block carver passes one FileChunk per chunk
		// (ID={FileID}/{FileOffset}, Hash, DataSize, State=Pending); legacy callers
		// pass nil and write no rows. CommitCarvedChunks also re-materializes
		// File.Blocks in this same txn so raw-row readers (snapshot, audit) stay
		// coherent. Superseded-row reaping happens once per carve pass
		// (ReapSupersededManifest), not per batch — see below.
		return CommitCarvedChunks(ctx, tx, payloadIDFromChunks(fileChunks), fileChunks)
	})
}

// CommitCarvedChunks writes a carve batch's fresh manifest rows and projects
// them into File.Blocks, in the caller's txn.
//
// A row is keyed by the file offset of its first claimed byte and Put is an
// upsert, so a fresh row landing on an offset an existing row already occupies
// replaces it. Whatever that row still owned past the fresh one is kept by
// PreserveClobberedRow, which runs before the batch and while the row is still
// there; by the time a batch reaches here the replacement is the intended
// outcome.
func CommitCarvedChunks(ctx context.Context, tx Transaction, payloadID string, rows []*block.FileChunk) error {
	for _, fc := range rows {
		if fc == nil {
			continue
		}
		if err := tx.Put(ctx, fc); err != nil {
			return fmt.Errorf("commit carved chunks: put %s: %w", fc.ID, err)
		}
	}
	return ProjectCommittedChunks(ctx, tx, payloadID, rows)
}

// PreserveClobberedRow keeps what the manifest row keyed at runStart still owns
// past runEnd, before a carve run's first fresh chunk takes that key over. It is
// a no-op when no row sits at runStart, or when the row there does not reach
// past runEnd — the ordinary carve, where a run appends or re-covers at least as
// much as it replaces.
//
// owed names the ranges past runEnd that are still backed by the remote, and the
// preserved claim is confined to them. That confinement is the whole point: the
// replaced row may equally have been spanning a punched hole, whose bytes must
// read as zeros, and putting its pre-punch content back over that hole is its
// own silent corruption. The manifest cannot tell the two apart, which is why
// owed is computed from the journal's interval index and passed in.
//
// One row is written per owed range, each reading the original chunk from
// however far into it that range begins, so a claim broken up by holes comes
// back as the pieces that survive rather than as one span across them.
//
// A range whose key another row already occupies is left to that row when it
// reaches at least as far, since the preserved piece would add no coverage and
// cost that row its own.
func PreserveClobberedRow(ctx context.Context, tx Transaction, payloadID string, runStart, runEnd int64, owed [][2]int64) error {
	if payloadID == "" || len(owed) == 0 {
		return nil
	}
	old, err := readChunkRow(ctx, tx, fmt.Sprintf("%s/%d", payloadID, runStart))
	if err != nil || old == nil {
		return err
	}
	// The row was read by the key runStart builds, so runStart is its start.
	rowEnd := runStart + int64(old.DataSize)
	if rowEnd <= runEnd {
		return nil // the run re-covers everything this row claimed
	}
	var written []*block.FileChunk
	for _, sp := range owed {
		lo, hi := max(sp[0], runEnd), min(sp[1], rowEnd)
		if lo >= hi {
			continue
		}
		piece, ok := narrowOffHead(old, runStart, lo, hi)
		if !ok {
			continue // the claim will not fit the row's fields: leave it behind
		}
		occupant, err := readChunkRow(ctx, tx, piece.ID)
		if err != nil {
			return err
		}
		if occupant != nil && lo+int64(occupant.DataSize) >= hi {
			continue
		}
		if err := tx.Put(ctx, piece); err != nil {
			return fmt.Errorf("preserve clobbered row %s: put %s: %w", old.ID, piece.ID, err)
		}
		written = append(written, piece)
	}
	if len(written) == 0 {
		return nil
	}
	return ProjectCommittedChunks(ctx, tx, payloadID, written)
}

// readChunkRow returns the row stored under id, or (nil, nil) when there is
// none. Absence is the common answer on this path — a run usually starts where
// no row does — so it is not an error.
func readChunkRow(ctx context.Context, tx Transaction, id string) (*block.FileChunk, error) {
	row, err := tx.GetFileChunk(ctx, id)
	if err != nil {
		if errors.Is(err, block.ErrFileChunkNotFound) || IsNotFoundError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read manifest row %s: %w", id, err)
	}
	return row, nil
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

// spanFreeStart returns the first byte of [rowStart, rowEnd) that no span
// claims, or rowEnd when the spans cover the row outright. Spans are disjoint
// and ascending, so the search finds the only span that can cover rowStart, and
// from there only the next span can begin exactly where the previous one ended.
func spanFreeStart(ordered [][2]int64, rowStart, rowEnd int64) int64 {
	start := rowStart
	i := sort.Search(len(ordered), func(j int) bool { return ordered[j][1] > start })
	for ; i < len(ordered) && ordered[i][0] <= start; i++ {
		start = ordered[i][1]
		if start >= rowEnd {
			return rowEnd
		}
	}
	return start
}

// narrowOffHead returns a copy of r claiming only [head, rowEnd) of the file,
// keyed at head and reading the chunk from that many bytes further in. The
// caller establishes rowStart < head < rowEnd, so what survives is a non-empty
// claim smaller than the one it replaces. It reports false when what the row
// gives up pushes its in-chunk start past what the 32-bit field holds, which no
// real chunk reaches — a chunk is bounded by the carver's maximum size — so
// refusing there simply keeps the row as it stands rather than wrapping.
func narrowOffHead(r *block.FileChunk, rowStart, head, rowEnd int64) (*block.FileChunk, bool) {
	start := int64(r.StartOffset) + (head - rowStart)
	if start > math.MaxUint32 {
		return nil, false
	}
	narrowed := *r
	narrowed.ID = fmt.Sprintf("%s/%d", chunkPayloadID(r.ID), head)
	narrowed.StartOffset = uint32(start)
	narrowed.DataSize = uint32(rowEnd - head)
	return &narrowed, true
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
// A row that STARTS inside its span and reaches past the end is narrowed off its
// HEAD instead: its ID moves to the span's end, StartOffset advances by what it
// gave up, and DataSize shrinks to match, so it claims [spanEnd, rowEnd) and
// nothing more. Sparing it whole would leave it the greatest covering start over
// [rowStart, spanEnd) — it would outrank the fresh rows there and serve the
// content those bytes held before the carve — and deleting it would strand
// [spanEnd, rowEnd) with no cover at all. Narrowing off the head is neither: the
// chunk on the remote keeps every byte and is still hash-verified over all of
// them, exactly as it is when a row is narrowed off its tail.
//
// A row that starts before its span and reaches past the end still stays whole,
// and is safe there: coverage resolves an overlap to the greatest covering
// start, and every fresh row inside the span starts later than a row that began
// before it, so the fresh rows win across the whole span and the survivor serves
// only its un-recarved remainder.
//
// A head narrow moves the row to a new manifest key, and a row already sitting
// at that key would be overwritten — so the move is taken only when the key is
// free. ponytail: an occupied key leaves the row spared whole and its stale
// cover in place; reaching it needs two pre-existing rows overlapping at the
// span's end, so a resolution rule for that collision is worth building only if
// one is ever observed.
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
	// The keys the manifest already holds, so a head narrow never moves a row
	// onto one.
	occupied := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		if r != nil {
			occupied[r.ID] = struct{}{}
		}
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
		head := spanFreeStart(ordered, rowStart, rowEnd)
		if head == rowStart {
			// The row starts where no span claims it, so what a span can take is
			// its tail. The span that acts on it is the first one whose end the row
			// does not reach past: an earlier span the row outreaches is one that
			// would strand the stretch beyond if acted on, and a later span the row
			// cannot reach, because acting here leaves it ending at or before this
			// span's start. Spans ascend, so this is a binary search.
			i := sort.Search(len(ordered), func(j int) bool { return ordered[j][1] >= rowEnd })
			if i == len(ordered) || ordered[i][0] >= rowEnd || ordered[i][1] <= rowStart {
				continue // no span acts on it: untouched (incl. cold remainders)
			}
			narrowed := *r
			narrowed.DataSize = uint32(ordered[i][0] - rowStart)
			if err := tx.Put(ctx, &narrowed); err != nil {
				return fmt.Errorf("reap superseded: narrow %s: %w", r.ID, err)
			}
			continue
		}
		// The row starts inside a span, so what the span takes is its head.
		if _, isNew := newOffsets[rowStart]; isNew {
			continue // a row this pass just wrote — keep it
		}
		if head >= rowEnd {
			if err := tx.Delete(ctx, r.ID); err != nil {
				return fmt.Errorf("reap superseded: delete %s: %w", r.ID, err)
			}
			continue
		}
		// It reaches past the span too, so it keeps only what lies past it,
		// re-keyed at the first byte it still claims.
		narrowed, ok := narrowOffHead(r, rowStart, head, rowEnd)
		if !ok {
			continue // the claim will not fit the row's fields: leave it as it stands
		}
		if _, taken := occupied[narrowed.ID]; taken {
			continue // see the head-narrow note above: spared rather than overwritten
		}
		if err := tx.Delete(ctx, r.ID); err != nil {
			return fmt.Errorf("reap superseded: unkey %s: %w", r.ID, err)
		}
		if err := tx.Put(ctx, narrowed); err != nil {
			return fmt.Errorf("reap superseded: narrow %s off its head: %w", r.ID, err)
		}
		occupied[narrowed.ID] = struct{}{}
	}
	return ProjectManifestToBlocks(ctx, tx, payloadID)
}
