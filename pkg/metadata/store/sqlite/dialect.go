package sqlite

import (
	"database/sql"
	"errors"

	storesql "github.com/marmos91/dittofs/pkg/metadata/store/sql"
)

// dialect supplies SQLite statement text and database/sql error classification
// to the shared SQL core. It is stateless, so one package-level value serves
// every store and transaction.
type dialect struct{}

var sqliteDialect dialect

// IsNoRows reports database/sql's empty-result sentinel.
func (dialect) IsNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// Chunks returns the SQLite file-chunk statements. They differ from postgres in
// placeholder syntax (?N against $N), the floor function (MAX against GREATEST)
// and the byte-ordering collation name (BINARY against "C") — text only; the
// logic that runs them is shared.
func (dialect) Chunks() *storesql.ChunkQueries { return &chunkQueries }

var chunkQueries = storesql.ChunkQueries{
	SelectByID:   `SELECT ` + fileChunkColumns + ` FROM file_blocks WHERE id = ?1`,
	SelectByHash: `SELECT ` + fileChunkColumns + ` FROM file_blocks WHERE hash = ?1 AND state = 2 /* Remote */`,
	Upsert:       putFileChunkQuery,
	Delete:       `DELETE FROM file_blocks WHERE id = ?1`,
	IncrementRef: `UPDATE file_blocks SET ref_count = ref_count + 1 WHERE id = ?1`,
	DecrementRef: `UPDATE file_blocks SET ref_count = MAX(ref_count - 1, 0) WHERE id = ?1 RETURNING ref_count`,
	// state = 2 (Remote) scoping mirrors SelectByHash and the memory/badger
	// backends: a Pending row is not a valid dedup donor.
	AddRef:             `UPDATE file_blocks SET ref_count = ref_count + 1 WHERE hash = ?1 AND state = 2 /* Remote */`,
	ReapZeroRef:        `DELETE FROM file_blocks WHERE id = ?1 AND ref_count = 0`,
	ListByPayloadRange: listFileChunksQuery,
	EnumerateHashes:    enumerateHashesQuery,
}

var _ storesql.Dialect = dialect{}
