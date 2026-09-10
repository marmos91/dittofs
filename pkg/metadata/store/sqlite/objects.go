// FileChunkStore support for the SQLite metadata store: content-addressed
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
package sqlite

import (
	"github.com/marmos91/dittofs/pkg/block"

	storesql "github.com/marmos91/dittofs/pkg/metadata/store/sql"
)

// Both the store and its transaction satisfy the interface through the
// promoted Core methods.
var _ block.FileChunkStore = (*SQLiteMetadataStore)(nil)
var _ block.FileChunkStore = (*sqliteTransaction)(nil)

const insertFileChunk = `INSERT INTO file_blocks (` + storesql.FileChunkColumns + `) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)`

// listFileChunksQuery is ChunkQueries.ListByPayloadRange for sqlite; what the
// bounds mean, and why the byte collation, lives on that field.
const listFileChunksQuery = `SELECT ` + storesql.FileChunkColumns + `
	FROM file_blocks
	WHERE id >= ?1 COLLATE BINARY AND id < ?2 COLLATE BINARY
	ORDER BY id COLLATE BINARY ASC`

// enumerateHashesQuery is ChunkQueries.EnumerateHashes for sqlite; what the
// union must cover, and why UNION ALL, lives on that field. The manifest arm
// renders the BLOB hash column as hex with lower(hex(...)).
const enumerateHashesQuery = `SELECT hash FROM file_blocks
UNION ALL
SELECT lower(hex(fbr.hash)) FROM file_block_refs fbr
JOIN inodes i ON fbr.file_id = i.id
WHERE i.nlink > 0`
