package postgres

import (
	"errors"

	"github.com/jackc/pgx/v5"

	storesql "github.com/marmos91/dittofs/pkg/metadata/store/sql"
)

// dialect supplies postgres statement text and postgres error classification to
// the shared SQL core. It is stateless, so one package-level value serves every
// store and transaction.
type dialect struct{}

var pgDialect dialect

// IsNoRows reports pgx's empty-result sentinel.
func (dialect) IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// Chunks returns the postgres file-chunk statements.
func (dialect) Chunks() *storesql.ChunkQueries { return &chunkQueries }

var chunkQueries = storesql.ChunkQueries{
	SelectByID:   selectFileChunkByID,
	SelectByHash: selectFileChunkByHash,
	Upsert:       putFileChunkQuery,
	Delete:       `DELETE FROM file_blocks WHERE id = $1`,
	IncrementRef: `UPDATE file_blocks SET ref_count = ref_count + 1 WHERE id = $1`,
	DecrementRef: `UPDATE file_blocks SET ref_count = GREATEST(ref_count - 1, 0) WHERE id = $1 RETURNING ref_count`,
	// state = 2 (Remote) scoping mirrors SelectByHash and the memory/badger
	// backends: a Pending row is not a valid dedup donor.
	AddRef:             `UPDATE file_blocks SET ref_count = ref_count + 1 WHERE hash = $1 AND state = 2 /* Remote */`,
	ReapZeroRef:        `DELETE FROM file_blocks WHERE id = $1 AND ref_count = 0`,
	ListByPayloadRange: listFileChunksQuery,
	EnumerateHashes:    enumerateHashesQuery,
}

var _ storesql.Dialect = dialect{}
