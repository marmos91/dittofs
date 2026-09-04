// Block-record support for the SQLite metadata store. The statements and the
// bodies that run them live in store/sql, promoted onto both the store and its
// transaction through the embedded Core; only CommitBlock stays here, because
// it needs a Transactor to open the transaction it commits in and Core is not
// one.
package sqlite

import (
	"context"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Compile-time assertions: the store and its transaction both satisfy the
// interface. Both reach it through the promoted Core methods, so a signature
// drift there fails here before any test runs.
var _ metadata.BlockRecordStore = (*SQLiteMetadataStore)(nil)
var _ metadata.BlockRecordStore = (*sqliteTransaction)(nil)

// CommitBlock atomically writes rec and marks each chunk synced within a single
// transaction. Idempotent on BlockID.
func (s *SQLiteMetadataStore) CommitBlock(ctx context.Context, rec block.BlockRecord, chunks []block.BlockChunkCommit) error {
	return metadata.DefaultCommitBlock(ctx, s, rec, chunks, nil)
}
