// FileChunkStore support for the SQLite metadata store: content-addressed file
// chunk tracking for deduplication and caching, over the file_blocks table.
//
// The statements and the bodies that run them live in store/sql, promoted onto
// both the store and its transaction through the embedded Core. What stays here
// is the dialect's own SQL text and the two entry points that must not be
// promoted: DecrementRefCountAndReap, which needs a transaction Core is not one
// of, and DecrementRefCountAndReapMany, whose two statements are only atomic
// when they share the transaction the caller opened.
package sqlite

import (
	"context"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"

	storesql "github.com/marmos91/dittofs/pkg/metadata/store/sql"
)

// Both the store and its transaction satisfy the interface through the
// promoted Core methods.
var _ block.FileChunkStore = (*SQLiteMetadataStore)(nil)
var _ block.FileChunkStore = (*sqliteTransaction)(nil)

// insertFileChunk is the INSERT the row writers share; each appends its own
// ON CONFLICT clause.
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

// DecrementRefCountAndReap atomically decrements ref_count and, when it hits 0,
// deletes the row — both statements run inside ONE transaction so the
// decrement-and-reap is atomic and TOCTOU-free against a concurrent AddRef
// (which takes the same row lock). Returns (0, nil) when the row is already
// absent — a swept row is not a caller error. Running through WithTransaction
// also gives a SQLITE_BUSY collision the package's bounded retry instead of
// surfacing it as a hard error.
func (s *SQLiteMetadataStore) DecrementRefCountAndReap(ctx context.Context, id string) (uint32, error) {
	var newCount uint32
	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var txErr error
		newCount, txErr = tx.DecrementRefCountAndReap(ctx, id)
		return txErr
	})
	if err != nil {
		return 0, err
	}
	return newCount, nil
}

// DecrementRefCountAndReapMany runs the batched decrement + reap on this
// transaction's executor so a subsequent rollback undoes the whole set. Going
// through the pool would let the two statements autocommit separately and
// survive that rollback.
func (tx *sqliteTransaction) DecrementRefCountAndReapMany(ctx context.Context, ids []string) error {
	return storesql.DecrementAndReapMany(ctx, tx.tx, sqliteDialect, ids)
}

// AddRef bumps RefCount on the row(s) indexed by the given content hash,
// implementing the FileChunkStore.AddRef contract the in-memory dedup LRU hit
// path uses. Returns metadata.ErrUnknownHash when no row matches; callers fall
// back to the full Put path on that sentinel. The reasoning about atomicity,
// Remote-only scoping and multi-row tolerance lives on the shared
// implementation in store/sql.
func (s *SQLiteMetadataStore) AddRef(ctx context.Context, hash block.ContentHash, _ string, _ block.ChunkRef) error {
	// payloadID + blockRef are accepted for future GC traceability; this
	// backend records ref count only, so they are intentionally blanked.
	return s.Core.AddRef(ctx, hash)
}

// AddRef bumps ref_count keyed by hash on the active transaction so a
// subsequent rollback undoes it. Returns metadata.ErrUnknownHash when no row
// matches.
func (tx *sqliteTransaction) AddRef(ctx context.Context, hash block.ContentHash, _ string, _ block.ChunkRef) error {
	// payloadID + blockRef are accepted for future GC traceability; this
	// backend records ref count only, so they are intentionally blanked.
	return tx.Core.AddRef(ctx, hash)
}
