package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// RepairKind names what one repair entry does to the manifest.
type RepairKind string

const (
	// RepairReplaceRow moves a row that carries no parseable chunk offset to
	// the offset the file's own block list gives its hash. Everything else
	// about the row — hash, size, refcount, state, timestamps — is carried
	// over; only the ID changes. Two defects go away at once: the row becomes
	// placeable and the range it belongs to becomes covered.
	RepairReplaceRow RepairKind = "replace_row"

	// RepairRecreateRow adds a manifest row for a range the file claims and
	// no row covers, taking the hash and length from the claim itself. It is
	// only ever proposed for a hash the synced-hash store resolves, so the
	// bytes are fetchable from the remote by that hash alone.
	RepairRecreateRow RepairKind = "recreate_row"
)

// RepairAction is one proposed manifest write. A scan that plans repairs fills
// Kind through ToRowID; a scan that applies them also fills Applied or
// SkipReason, so the same value describes both what was intended and what
// happened.
type RepairAction struct {
	// Kind is the action taken on the manifest.
	Kind RepairKind `json:"kind"`

	// Path and PayloadID identify the file the row belongs to.
	Path      string `json:"path"`
	PayloadID string `json:"payload_id"`

	// Offset and Size are the byte range the repaired row will claim, taken
	// from the file's own block list.
	Offset uint64 `json:"offset"`
	Size   uint32 `json:"size"`

	// Hash is the content hash the row carries. The remote read path
	// resolves and verifies a chunk by this hash alone, so a repair that
	// named the wrong bytes would fail the read rather than serve them.
	Hash string `json:"hash"`

	// FromRowID is the unplaceable row being moved, empty for a recreate.
	FromRowID string `json:"from_row_id,omitempty"`

	// ToRowID is the manifest key the row will live under.
	ToRowID string `json:"to_row_id"`

	// Applied reports that the write went through.
	Applied bool `json:"applied,omitempty"`

	// SkipReason names why the action was not applied. An action is skipped
	// when a precondition that held during the scan no longer holds inside
	// the write transaction — the store is live and may have moved.
	SkipReason string `json:"skip_reason,omitempty"`
}

// chunkRowID renders the manifest key a chunk at off within payloadID lives
// under. It is the inverse of block.ParseChunkOffset.
func chunkRowID(payloadID string, off uint64) string {
	return fmt.Sprintf("%s/%d", payloadID, off)
}

// refKey groups a claim and a row by the two properties that have to match
// for one to stand in for the other.
type refKey struct {
	hash block.ContentHash
	size uint32
}

// planPayloadRepairs proposes the manifest writes that would make one payload
// readable again. It proposes a write only where the evidence names both the
// bytes and where they belong:
//
//   - the file's own block list gives a hash, an offset and a length for the
//     range, and no placeable row covers that range, so writing a row there
//     takes nothing away from any reader; and
//   - either an unplaceable row carries exactly that hash and length — its
//     bytes are the claim's bytes, only its offset was lost — or the
//     synced-hash store resolves the hash, so the remote holds the bytes.
//
// Everything else is left alone. A claim whose hash nothing can resolve gets
// no row: a row pointing at bytes no store holds turns an uncovered range into
// a failing read without recovering anything. An unplaceable row that matches
// no claim gets no offset: there is nothing that says where it belongs.
//
// Ambiguity is refused rather than guessed. Where a hash and length pair names
// more than one uncovered claim, or more than one unplaceable row, the two
// cannot be paired one-to-one and the rows are left for the operator; those
// claims fall through to the synced-hash branch, which needs no pairing.
func planPayloadRepairs(
	ctx context.Context,
	store metadata.Store,
	path, payloadID string,
	f *metadata.File,
	rowIDs map[string]struct{},
	unplaceable []*block.FileChunk,
	covered [][2]uint64,
	checkSynced bool,
) ([]RepairAction, error) {
	candidates := make([]block.ChunkRef, 0, 4)
	perKey := make(map[refKey]int)
	for _, ref := range f.Blocks {
		if !repairableClaim(ref, f.Size, rowIDs, payloadID, covered) {
			continue
		}
		candidates = append(candidates, ref)
		perKey[refKey{ref.Hash, ref.Size}]++
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	rowsByKey := make(map[refKey][]*block.FileChunk, len(unplaceable))
	for _, r := range unplaceable {
		if r.Hash.IsZero() {
			continue // pending row: no committed bytes to place anywhere
		}
		k := refKey{r.Hash, r.DataSize}
		rowsByKey[k] = append(rowsByKey[k], r)
	}

	out := make([]RepairAction, 0, len(candidates))
	for _, ref := range candidates {
		k := refKey{ref.Hash, ref.Size}
		action := RepairAction{
			Path:      path,
			PayloadID: payloadID,
			Offset:    ref.Offset,
			Size:      ref.Size,
			Hash:      ref.Hash.String(),
			ToRowID:   chunkRowID(payloadID, ref.Offset),
		}
		switch rows := rowsByKey[k]; {
		case len(rows) == 1 && perKey[k] == 1:
			action.Kind = RepairReplaceRow
			action.FromRowID = rows[0].ID
		case checkSynced:
			synced, err := store.IsSynced(ctx, ref.Hash)
			if err != nil {
				return nil, fmt.Errorf("is synced %s: %w", ref.Hash, err)
			}
			if !synced {
				continue
			}
			action.Kind = RepairRecreateRow
		default:
			continue
		}
		out = append(out, action)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// repairableClaim reports whether one entry of a file's block list names a
// range a repair may write a row for: it has to carry committed bytes, sit
// wholly inside the file, be covered by no placeable row, and have nothing
// already occupying its manifest key.
func repairableClaim(
	ref block.ChunkRef,
	size uint64,
	rowIDs map[string]struct{},
	payloadID string,
	covered [][2]uint64,
) bool {
	if ref.Size == 0 || ref.Hash.IsZero() {
		return false
	}
	end := ref.Offset + uint64(ref.Size)
	if end < ref.Offset || end > size {
		// A claim reaching past the recorded size is a disagreement the scan
		// cannot settle: writing a row for it would commit to one side of it.
		return false
	}
	if overlapsCovered(covered, ref.Offset, end) {
		return false
	}
	// A row already at that key holds no committed bytes yet — a pending
	// rollup owns the offset, and overwriting it would drop the write it is
	// about to commit.
	_, taken := rowIDs[chunkRowID(payloadID, ref.Offset)]
	return !taken
}

// overlapsCovered reports whether [start, end) touches any covered extent.
// covered must be sorted and non-overlapping.
//
// ponytail: linear over the payload's extents, and it only runs for a payload
// that already has a claimed-uncovered range; make it a binary search if a
// store ever shows up whose damaged files are also its most fragmented.
func overlapsCovered(covered [][2]uint64, start, end uint64) bool {
	for _, c := range covered {
		if c[0] < end && start < c[1] {
			return true
		}
	}
	return false
}

// applyPayloadRepairs writes one payload's planned actions in a single
// transaction and records, on each action, whether it landed.
//
// Every precondition the plan relied on is re-established against the rows and
// the file the transaction sees, because the plan was computed outside it and
// the share stays live throughout: a concurrent carve may have written the row
// the plan wanted to add, and a concurrent truncate may have dropped the claim
// it was derived from. An action whose evidence no longer holds is skipped with
// a reason, never forced.
func applyPayloadRepairs(ctx context.Context, store metadata.Store, f *metadata.File, actions []RepairAction) error {
	handle, err := metadata.EncodeFileHandle(f)
	if err != nil {
		return fmt.Errorf("encode handle for payload %q: %w", f.PayloadID, err)
	}
	payloadID := string(f.PayloadID)

	return store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		cur, err := tx.GetFile(ctx, handle)
		if metadata.IsNotFoundError(err) {
			// Unlinked between the scan and the write. There is nothing left
			// to repair, and it is not a reason to abandon the other payloads.
			for i := range actions {
				actions[i].SkipReason = "the file is gone"
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("re-read file for payload %q: %w", payloadID, err)
		}
		rows, err := tx.ListFileChunks(ctx, payloadID)
		if err != nil {
			return fmt.Errorf("re-list chunks for payload %q: %w", payloadID, err)
		}

		byID := make(map[string]*block.FileChunk, len(rows))
		covered := make([][2]uint64, 0, len(rows))
		for _, row := range rows {
			if row == nil {
				continue
			}
			byID[row.ID] = row
			off, placeable := block.ParseChunkOffset(row.ID)
			if !placeable || row.Hash.IsZero() {
				continue
			}
			if e, ok := clipRange(off, uint64(row.DataSize), cur.Size); ok {
				covered = append(covered, e)
			}
		}
		covered = coalesceExtents(covered)

		claimed := make(map[refKey]map[uint64]struct{}, len(cur.Blocks))
		for _, ref := range cur.Blocks {
			k := refKey{ref.Hash, ref.Size}
			if claimed[k] == nil {
				claimed[k] = make(map[uint64]struct{}, 1)
			}
			claimed[k][ref.Offset] = struct{}{}
		}

		now := time.Now().UTC()
		for i := range actions {
			a := &actions[i]
			row, err := repairRow(ctx, tx, a, cur, byID, claimed, covered, now)
			if err != nil {
				return err
			}
			if row == nil {
				continue // a.SkipReason says why
			}
			if a.Kind == RepairReplaceRow {
				// The delete and the put share this transaction, so the row
				// is never absent from the manifest between the two.
				if err := tx.Delete(ctx, a.FromRowID); err != nil {
					return fmt.Errorf("delete unplaceable row %q: %w", a.FromRowID, err)
				}
			}
			if err := tx.Put(ctx, row); err != nil {
				return fmt.Errorf("put repaired row %q: %w", a.ToRowID, err)
			}
			// Fold the write into the view the remaining actions are checked
			// against, so a second action for the same offset — two block-list
			// entries claiming one range — is skipped as occupied rather than
			// silently overwriting the first.
			byID[row.ID] = row
			delete(byID, a.FromRowID)
			covered = coalesceExtents(append(covered, [2]uint64{a.Offset, a.Offset + uint64(a.Size)}))
			// The file's block list already carries this claim — it is where
			// the repair came from — so its projection needs no rewrite.
			a.Applied = true
		}
		return nil
	})
}

// repairRow re-checks one action against the transaction's view and returns
// the row to write, or nil with a.SkipReason set when the evidence has moved.
func repairRow(
	ctx context.Context,
	tx metadata.Transaction,
	a *RepairAction,
	cur *metadata.File,
	byID map[string]*block.FileChunk,
	claimed map[refKey]map[uint64]struct{},
	covered [][2]uint64,
	now time.Time,
) (*block.FileChunk, error) {
	hash, err := block.ParseContentHash(a.Hash)
	if err != nil {
		return nil, fmt.Errorf("parse planned hash %q: %w", a.Hash, err)
	}
	k := refKey{hash, a.Size}

	if _, ok := claimed[k][a.Offset]; !ok {
		a.SkipReason = "the file no longer claims this range"
		return nil, nil
	}
	if end := a.Offset + uint64(a.Size); end > cur.Size {
		a.SkipReason = "the file is now shorter than the range"
		return nil, nil
	}
	if _, taken := byID[a.ToRowID]; taken {
		a.SkipReason = "a manifest row now occupies the target offset"
		return nil, nil
	}
	if overlapsCovered(covered, a.Offset, a.Offset+uint64(a.Size)) {
		a.SkipReason = "another row now covers the range"
		return nil, nil
	}

	if a.Kind == RepairReplaceRow {
		src, ok := byID[a.FromRowID]
		if !ok {
			a.SkipReason = "the unplaceable row is gone"
			return nil, nil
		}
		if src.Hash != hash || src.DataSize != a.Size {
			a.SkipReason = "the unplaceable row no longer matches the claim"
			return nil, nil
		}
		moved := *src
		moved.ID = a.ToRowID
		return &moved, nil
	}

	// The synced marker is the whole of a recreate's evidence, so it is read
	// again here rather than trusted from the plan. It can be dropped between
	// the two — the refcount cascade clears the marker of a hash nothing
	// references any more, and a hash whose row went missing is exactly that.
	synced, err := tx.IsSynced(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("re-check synced %s: %w", hash, err)
	}
	if !synced {
		a.SkipReason = "the claim's hash is no longer marked synced"
		return nil, nil
	}

	// A recreated row states what the evidence supports and no more: the
	// claim's hash and its length, under the manifest key its offset gives.
	// Those three are what a read needs — it resolves the bytes by hash
	// through the synced-hash store and verifies their BLAKE3 on arrival, so
	// a row naming a chunk the remote does not hold fails the read rather
	// than serving it, and one naming the wrong chunk fails the verify.
	// State and refcount are left where the carve path leaves them, which is
	// where every other row in the store sits.
	return &block.FileChunk{
		ID:         a.ToRowID,
		Hash:       hash,
		DataSize:   a.Size,
		State:      block.BlockStatePending,
		CreatedAt:  now,
		LastAccess: now,
	}, nil
}
