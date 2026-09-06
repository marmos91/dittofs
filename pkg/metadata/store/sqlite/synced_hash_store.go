// Synced-hash support for the SQLite metadata store. The statements and the
// bodies that run them live in store/sql, promoted onto both the store and its
// transaction through the embedded Core. One method stays here:
// PutSyncedLocators, which spans several statements for a commit large enough
// to batch, so store/sql exposes it as a function over an executor rather than
// as a promoted method the pool-backed store would autocommit piecemeal.
package sqlite

import (
	"context"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"

	storesql "github.com/marmos91/dittofs/pkg/metadata/store/sql"
)

// Both the store and its transaction satisfy the interface through the
// promoted Core methods. Within a transaction SQLite gives read-your-writes, so
// a MarkSynced after a DeleteSynced in the same tx records the new locator.
var (
	_ metadata.SyncedHashStore = (*SQLiteMetadataStore)(nil)
	_ metadata.SyncedHashStore = (*sqliteTransaction)(nil)
)

// PutSyncedLocators writes the marker and locator of every chunk inside this
// transaction, last-wins per hash.
func (tx *sqliteTransaction) PutSyncedLocators(ctx context.Context, chunks []block.BlockChunkCommit) error {
	return storesql.PutSyncedLocators(ctx, tx.X, tx.D, chunks)
}
