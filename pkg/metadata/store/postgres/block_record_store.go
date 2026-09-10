// Block-record support for the PostgreSQL metadata store. Nothing
// dialect-specific is left: the statements and the bodies that run them live in
// store/sql, promoted onto the transaction through the embedded Core and onto
// the store through PoolPath, which also carries the two writes that need a
// transaction of their own (CommitBlock and DecrLiveChunkCount).
//
// What remains here is the assertion that both halves still satisfy the
// interface. It is worth a file of its own: the methods now arrive by
// promotion, so a change in store/sql that dropped one would otherwise be
// caught at the first call site rather than at the definition.
package postgres

import (
	"github.com/marmos91/dittofs/pkg/metadata"
)

var _ metadata.BlockRecordStore = (*PostgresMetadataStore)(nil)
var _ metadata.BlockRecordStore = (*postgresTransaction)(nil)
