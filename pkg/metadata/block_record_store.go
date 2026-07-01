package metadata

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/block"
)

// ErrCommitPartial is returned by DefaultCommitBlock when the block record
// transaction committed successfully but the MarkSynced loop did not fully
// complete. BlockID identifies the committed (but only partially-synced) block.
// Callers should retry DefaultCommitBlock with the same BlockID and BlockRecord
// — the idempotent transaction guard skips the transaction and re-runs the
// MarkSynced loop to completion.
type ErrCommitPartial struct {
	BlockID string
	Cause   error
}

func (e *ErrCommitPartial) Error() string {
	return fmt.Sprintf("commit block %s: mark-synced incomplete: %v", e.BlockID, e.Cause)
}

func (e *ErrCommitPartial) Unwrap() error { return e.Cause }

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

// DefaultCommitBlock atomically writes a block record and all associated local
// chunk locations within a single transaction, then (outside the tx) marks
// each chunk synced via MarkSynced. Idempotent: if the block record already
// exists the function is a no-op (LiveChunkCount is not double-counted).
//
// Exported so Store implementations in sub-packages can delegate CommitBlock
// to this shared logic.
func DefaultCommitBlock(
	ctx context.Context,
	s interface {
		Transactor
		SyncedHashStore
	},
	rec block.BlockRecord,
	chunks []block.BlockChunkCommit,
) error {
	if err := s.WithTransaction(ctx, func(tx Transaction) error {
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
		for _, c := range chunks {
			if err := tx.PutLocalLocation(ctx, c.Hash, c.Local); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	// MarkSynced runs unconditionally after the transaction commits — even when
	// the block record already existed (justCommitted would have been false in the
	// old guard). MarkSynced is idempotent, so a no-op on a fully-committed block
	// is safe. Crucially, this fixes the retry path: if a previous CommitBlock
	// call committed the tx but then failed mid-MarkSynced, the retry must still
	// reach this loop to write the pending remote locators.
	//
	// If MarkSynced fails here the transaction has ALREADY committed (block record
	// + local locations are durable). Return ErrCommitPartial so callers can retry
	// with the same BlockID — DefaultCommitBlock's idempotent tx guard will skip
	// the transaction and re-run the MarkSynced loop to completion without minting
	// a new block.
	for _, c := range chunks {
		if err := s.MarkSynced(ctx, c.Hash, c.Remote); err != nil {
			return &ErrCommitPartial{BlockID: rec.BlockID, Cause: err}
		}
	}
	return nil
}
