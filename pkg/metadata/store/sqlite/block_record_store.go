// Block-record support for the SQLite metadata store. The statements and the
// bodies that run them live in store/sql, promoted onto both the store and its
// transaction through the embedded Core. Two methods stay here: CommitBlock,
// which needs a Transactor to open the transaction it commits in and Core is
// not one, and DecrLiveChunkCount, which shadows its promoted namesake to keep
// the retry budget WithTransaction carries.
package sqlite

import (
	"context"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Both the store and its transaction satisfy the interface through the
// promoted Core methods.
var _ metadata.BlockRecordStore = (*SQLiteMetadataStore)(nil)
var _ metadata.BlockRecordStore = (*sqliteTransaction)(nil)

// CommitBlock atomically writes rec and marks each chunk synced within a single
// transaction. Idempotent on BlockID.
func (s *SQLiteMetadataStore) CommitBlock(ctx context.Context, rec block.BlockRecord, chunks []block.BlockChunkCommit) error {
	return metadata.DefaultCommitBlock(ctx, s, rec, chunks, nil)
}

// DecrLiveChunkCount shadows the promoted Core method so the statement runs
// inside WithTransaction, and so inherits its busy/conflict retry budget.
//
// The other block-record writes can afford to surface a transient conflict:
// their caller retries the whole sweep. This one cannot. Block reclamation
// clears the synced marker before it decrements, so a re-visit resolves the
// block as unsynced and skips the decrement entirely — a decrement lost to a
// transient conflict is lost for good, and the block it belonged to is never
// reclaimed.
func (s *SQLiteMetadataStore) DecrLiveChunkCount(ctx context.Context, blockID string, delta uint32) (uint32, error) {
	var remaining uint32
	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		remaining, err = tx.DecrLiveChunkCount(ctx, blockID, delta)
		return err
	})
	return remaining, err
}
