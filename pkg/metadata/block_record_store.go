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

// ProjectManifestToBlocks re-materializes File.Blocks for payloadID from the
// current FileChunk manifest, within the caller's txn. Every manifest mutation
// (carve commit, #953 reap + re-carve straddle, truncate) must call this in the
// SAME txn that changed the rows, so File.Blocks == projection(rows) always and
// the raw-row readers (snapshot WriteSnapshot, refcount audit) never see drift.
// A missing file (deleted concurrently) is a no-op. ponytail: this is the
// switchover bridge; the #1715 fb-split removes File.Blocks from the row entirely
// and derives at read time, retiring this projection.
func ProjectManifestToBlocks(ctx context.Context, tx Transaction, payloadID string) error {
	if payloadID == "" {
		return nil
	}
	rows, err := tx.ListFileChunks(ctx, payloadID)
	if err != nil {
		return fmt.Errorf("project blocks: list manifest for %s: %w", payloadID, err)
	}
	file, err := tx.GetFileByPayloadID(ctx, PayloadID(payloadID))
	if err != nil {
		// No File row (block-layer fixtures with synthetic payloadIDs, or a file
		// deleted between carve and commit) → nothing to project onto, no-op.
		if IsNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("project blocks: get file for %s: %w", payloadID, err)
	}
	if file == nil {
		return nil
	}
	file.Blocks = ManifestToChunkRefs(rows)
	return tx.PutFile(ctx, file)
}

// payloadIDFromChunks extracts the shared payloadID from a carve pass's FileChunk
// rows (all rows of one carve belong to one file). Returns "" when the rows are
// nil/empty or malformed, which callers treat as "skip projection".
func payloadIDFromChunks(fileChunks []*block.FileChunk) string {
	for _, fc := range fileChunks {
		if fc == nil {
			continue
		}
		if i := strings.LastIndexByte(fc.ID, '/'); i > 0 {
			return fc.ID[:i]
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

// LocalChunkIndex maps content hashes to their position in a local log-blob.
// It mirrors SyncedHashStore in interface shape: idempotent put, safe miss on
// get, idempotent delete.
type LocalChunkIndex interface {
	// PutLocalLocation records or overwrites the local position for hash.
	PutLocalLocation(ctx context.Context, hash block.ContentHash, loc block.LocalChunkLocation) error

	// GetLocalLocation returns the local position for hash.
	// Returns (_, false, nil) when no entry exists.
	GetLocalLocation(ctx context.Context, hash block.ContentHash) (block.LocalChunkLocation, bool, error)

	// DeleteLocalLocation removes the local position for hash.
	// Idempotent: deleting an absent entry returns nil.
	DeleteLocalLocation(ctx context.Context, hash block.ContentHash) error
}

// DefaultCommitBlock atomically writes a block record, all associated local
// chunk locations, and every chunk's synced marker + remote locator within a
// SINGLE transaction. Either the whole commit is visible or none of it is —
// there is no partially-committed state to retry, so a commit error simply
// propagates to the caller (whose existing requeue logic re-drives the batch).
//
// Semantics:
//
//   - Idempotent on BlockID: if the block record already exists the function
//     is a no-op (LiveChunkCount is not double-counted, locators untouched).
//   - A chunk's local location is written only when its Local field is
//     non-zero. Migrated chunks (cas→blocks) have no local bytes; writing a
//     zero-valued location would make local reads resolve to empty bytes.
//   - Locator writes are LAST-WINS: DeleteSynced-then-MarkSynced inside the
//     tx overwrites any existing locator with the new block locator. The
//     direct MarkSynced method stays first-wins; CommitBlock needs overwrite
//     because the cas→blocks migration re-commits chunks whose standalone
//     (zero-BlockID) locators must be rewritten to point into the new block.
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
		for _, c := range chunks {
			if c.Local != (block.LocalChunkLocation{}) {
				if err := tx.PutLocalLocation(ctx, c.Hash, c.Local); err != nil {
					return err
				}
			}
			// DeleteSynced + MarkSynced = locator overwrite (last-wins), see
			// the function comment. MarkSynced alone would be first-wins and
			// leave a stale standalone locator in place.
			if err := tx.DeleteSynced(ctx, c.Hash); err != nil {
				return err
			}
			if err := tx.MarkSynced(ctx, c.Hash, c.Remote); err != nil {
				return err
			}
		}
		// Materialize File.Blocks from the manifest in this same txn so raw-row
		// readers (snapshot, audit) stay coherent. Skipped for legacy callers
		// that pass no fileChunks (nil payloadID).
		return ProjectManifestToBlocks(ctx, tx, payloadIDFromChunks(fileChunks))
	})
}
