// Synced-hash support for the PostgreSQL metadata store. Nothing
// dialect-specific is left: the statements and the bodies that run them live in
// store/sql, promoted onto the transaction through the embedded Core and onto
// the store through PoolPath, which overrides PutSyncedLocators so its several
// statements share one transaction instead of autocommitting on the pool.
//
// What remains here is the assertion that both halves still satisfy the
// interface. Postgres gives read-your-writes within a transaction, so a
// MarkSynced after a DeleteSynced in the same tx records the new locator.
package postgres

import (
	"github.com/marmos91/dittofs/pkg/metadata"
)

var (
	_ metadata.SyncedHashStore = (*PostgresMetadataStore)(nil)
	_ metadata.SyncedHashStore = (*postgresTransaction)(nil)
)
