// FileChunkStore support for the PostgreSQL metadata store: content-addressed
// file chunk tracking for deduplication and caching, over the file_blocks
// table.
//
// The bodies that run these statements live in store/sql, promoted onto both
// the store and its transaction through the embedded Core -- including AddRef,
// whose wide block.FileChunkStore signature Core now carries directly, and the
// batched decrement-and-reap, which PoolPath overrides so it cannot autocommit
// on the pool.
//
// What stays here is what genuinely differs between the backends: this
// dialect's own SQL text, and the assertion that both halves still satisfy the
// interface.
package postgres

import (
	"github.com/marmos91/dittofs/pkg/block"

	storesql "github.com/marmos91/dittofs/pkg/metadata/store/sql"
)

// Both the store and its transaction satisfy the interface through the
// promoted Core methods.
var _ block.FileChunkStore = (*PostgresMetadataStore)(nil)
var _ block.FileChunkStore = (*postgresTransaction)(nil)

const (
	selectFileChunkByID   = `SELECT ` + storesql.FileChunkColumns + ` FROM file_blocks WHERE id = $1`
	selectFileChunkByHash = `SELECT ` + storesql.FileChunkColumns + ` FROM file_blocks WHERE hash = $1 AND state = 2 /* Remote */`

	// insertFileChunk is the INSERT the row writers share; each appends its
	// own ON CONFLICT clause.
	insertFileChunk = `INSERT INTO file_blocks (` + storesql.FileChunkColumns + `) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
)

// listFileChunksQuery is ChunkQueries.ListByPayloadRange for postgres; what the
// bounds mean, and why the byte collation, lives on that field.
const listFileChunksQuery = `SELECT ` + storesql.FileChunkColumns + `
	FROM file_blocks
	WHERE id >= $1 COLLATE "C" AND id < $2 COLLATE "C"
	ORDER BY id COLLATE "C" ASC`

// enumerateHashesQuery is ChunkQueries.EnumerateHashes for postgres; what the
// union must cover, and why UNION ALL, lives on that field. The manifest arm
// renders the BYTEA hash column as hex with encode(..., 'hex').
const enumerateHashesQuery = `SELECT hash FROM file_blocks
UNION ALL
SELECT encode(fbr.hash, 'hex') FROM file_block_refs fbr
JOIN inodes i ON fbr.file_id = i.id
WHERE i.nlink > 0`
