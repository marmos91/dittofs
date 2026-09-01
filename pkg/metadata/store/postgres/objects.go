package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"

	storesql "github.com/marmos91/dittofs/pkg/metadata/store/sql"
)

// ============================================================================
// FileChunkStore Implementation for PostgreSQL Store
// ============================================================================
//
// This file implements the FileChunkStore interface for the PostgreSQL metadata store.
// It provides content-addressed file chunk tracking for deduplication and caching.
//
// The FileChunkStore interface is narrowed to 6 methods. The backend
// retains the legacy GetFileChunk + ListFileChunks helpers as
// concrete methods on the struct (not on the public interface) for
// engine-internal callers.
//
// Table:
//   - file_blocks: File block data with UUID as primary key and hash index
//
// Thread Safety: All operations use PostgreSQL transactions for ACID guarantees.
//
// ============================================================================

// Ensure PostgresMetadataStore implements FileChunkStore
var _ block.FileChunkStore = (*PostgresMetadataStore)(nil)

// ============================================================================
// FileChunk Operations
// ============================================================================

// fileChunkColumns is the file_blocks column list every chunk read and write
// names, in the order scanFileChunk reads them. Naming it once is what keeps a
// column added to one query from being missed by the others, or by the scan.
const fileChunkColumns = `id, hash, data_size, start_offset, ref_count, last_access, created_at, state, last_sync_attempt_at`

const (
	selectFileChunkByID   = `SELECT ` + fileChunkColumns + ` FROM file_blocks WHERE id = $1`
	selectFileChunkByHash = `SELECT ` + fileChunkColumns + ` FROM file_blocks WHERE hash = $1 AND state = 2 /* Remote */`
	insertFileChunk       = `INSERT INTO file_blocks (` + fileChunkColumns + `) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
)

// putFileChunkQuery omits ref_count from the ON CONFLICT UPDATE list so
// concurrent IncrementRefCount / DecrementRefCount (which run as atomic SQL
// `+1` / `-1` UPDATEs) cannot be silently overwritten by a stale
// Put-with-in-memory-RefCount. RefCount on the INSERT path is still set
// verbatim from the caller's *FileChunk (matches the contract for new rows).
// For existing rows, RefCount mutates exclusively through Increment/Decrement.
// hash uses COALESCE so a zero-hash Put never NULLs a previously-persisted good
// hash.
const putFileChunkQuery = insertFileChunk + `
	ON CONFLICT (id) DO UPDATE SET
		hash = COALESCE(EXCLUDED.hash, file_blocks.hash),
		data_size = EXCLUDED.data_size,
		start_offset = EXCLUDED.start_offset,
		last_access = EXCLUDED.last_access,
		state = EXCLUDED.state,
		last_sync_attempt_at = EXCLUDED.last_sync_attempt_at`

// GetFileChunk retrieves a file chunk by its ID. Not on the narrowed
// FileChunkStore interface; kept as a backend
// method for engine-internal callers.
func (s *PostgresMetadataStore) GetFileChunk(ctx context.Context, id string) (*metadata.FileChunk, error) {
	return getFileChunkTx(ctx, s.conn(), id)
}

// Put stores or updates a file chunk.
func (s *PostgresMetadataStore) Put(ctx context.Context, chunk *metadata.FileChunk) error {
	return putFileChunkTx(ctx, s.conn(), chunk)
}

// Delete removes a file chunk by its ID.
func (s *PostgresMetadataStore) Delete(ctx context.Context, id string) error {
	return deleteFileChunkTx(ctx, s.conn(), id)
}

// IncrementRefCount atomically increments a block's RefCount.
func (s *PostgresMetadataStore) IncrementRefCount(ctx context.Context, id string) error {
	return incrementRefCountTx(ctx, s.conn(), id)
}

// DecrementRefCount atomically decrements a block's RefCount.
func (s *PostgresMetadataStore) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	return decrementRefCountTx(ctx, s.conn(), id)
}

// DecrementRefCountAndReap atomically decrements ref_count and, when it hits 0,
// deletes the row — both statements run inside ONE transaction so the
// decrement-and-reap is atomic and TOCTOU-free against a concurrent AddRef
// (which takes the same row lock). Returns (0, nil) when the row is already
// absent — a swept row is not a caller error. Running through WithTransaction
// also gives a serialization failure or deadlock the package's bounded retry
// instead of surfacing it as a hard error.
func (s *PostgresMetadataStore) DecrementRefCountAndReap(ctx context.Context, id string) (uint32, error) {
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

// decrementAndReapTx runs the -1 UPDATE then a conditional DELETE. The
// `ref_count = 0` predicate on the DELETE means a bump that landed between the
// two statements leaves the row alive. Returns (0, nil) for an already-swept row
// rather than ErrFileChunkNotFound.
//
// Callers must supply a transaction Executor: the two statements are only
// atomic against a concurrent AddRef if they share one transaction.
func decrementAndReapTx(ctx context.Context, x storesql.Executor, id string) (uint32, error) {
	var newCount uint32
	err := x.QueryRow(ctx,
		`UPDATE file_blocks SET ref_count = GREATEST(ref_count - 1, 0) WHERE id = $1 RETURNING ref_count`,
		id).Scan(&newCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil // tolerate already-swept row
	}
	if err != nil {
		return 0, fmt.Errorf("decrement ref count: %w", err)
	}
	if newCount == 0 {
		if _, err := x.Exec(ctx,
			`DELETE FROM file_blocks WHERE id = $1 AND ref_count = 0`, id); err != nil {
			return 0, fmt.Errorf("reap zero-ref block: %w", err)
		}
	}
	return newCount, nil
}

// decrementAndReapManyTx applies the -1 UPDATE and the reap-at-zero DELETE to
// the whole id set in two statements, so a rollback undoes both. The
// `ref_count = 0` predicate means a bump that landed between the two leaves that
// row alive, and an id with no row is a no-op — the same outcomes
// decrementAndReapTx produces one row at a time.
func decrementAndReapManyTx(ctx context.Context, x storesql.Executor, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := x.Exec(ctx,
		`UPDATE file_blocks SET ref_count = GREATEST(ref_count - 1, 0) WHERE id = ANY($1)`,
		ids); err != nil {
		return fmt.Errorf("decrement ref count: %w", err)
	}
	if _, err := x.Exec(ctx,
		`DELETE FROM file_blocks WHERE id = ANY($1) AND ref_count = 0`,
		ids); err != nil {
		return fmt.Errorf("reap zero-ref block: %w", err)
	}
	return nil
}

// getFileChunkTx reads one chunk by id.
func getFileChunkTx(ctx context.Context, x storesql.Executor, id string) (*metadata.FileChunk, error) {
	chunk, err := scanFileChunk(x.QueryRow(ctx, selectFileChunkByID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, metadata.ErrFileChunkNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get file chunk: %w", err)
	}
	return chunk, nil
}

// putFileChunkTx stores or updates a chunk row.
//
// The hash column is persisted whenever the chunk carries a non-zero content
// hash, regardless of state. The content hash is derived at rollup time (long
// before the block reaches the remote) and is the key the engine's CAS read path
// uses to resolve a chunk. Gating the write on IsRemote() left every Pending row
// with a NULL hash; reads then survived only while the bytes stayed in the local
// append log or RAM cache, and broke the moment local state went cold (restart +
// cache eviction, or a snapshot restore's ResetLocalState). The memory and badger
// backends always store the hash inline on the row, so this matches them.
//
// LastSyncAttemptAt is persisted as NULL when zero so the janitor's
// `last_sync_attempt_at < cutoff` predicate excludes never-claimed rows
// naturally instead of matching every Pending row.
func putFileChunkTx(ctx context.Context, x storesql.Executor, chunk *metadata.FileChunk) error {
	var hashStr *string
	if !chunk.Hash.IsZero() {
		h := chunk.Hash.String()
		hashStr = &h
	}
	var lastSyncAttemptAt *time.Time
	if !chunk.LastSyncAttemptAt.IsZero() {
		t := chunk.LastSyncAttemptAt
		lastSyncAttemptAt = &t
	}
	if _, err := x.Exec(ctx, putFileChunkQuery,
		chunk.ID, hashStr, chunk.DataSize, chunk.StartOffset,
		chunk.RefCount, chunk.LastAccess, chunk.CreatedAt, chunk.State, lastSyncAttemptAt); err != nil {
		return fmt.Errorf("put file chunk: %w", err)
	}
	return nil
}

// deleteFileChunkTx removes one chunk row by id.
func deleteFileChunkTx(ctx context.Context, x storesql.Executor, id string) error {
	result, err := x.Exec(ctx, `DELETE FROM file_blocks WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete file chunk: %w", err)
	}
	if result.RowsAffected() == 0 {
		return metadata.ErrFileChunkNotFound
	}
	return nil
}

// incrementRefCountTx atomically bumps one chunk's RefCount.
func incrementRefCountTx(ctx context.Context, x storesql.Executor, id string) error {
	result, err := x.Exec(ctx,
		`UPDATE file_blocks SET ref_count = ref_count + 1 WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("increment ref count: %w", err)
	}
	if result.RowsAffected() == 0 {
		return metadata.ErrFileChunkNotFound
	}
	return nil
}

// decrementRefCountTx atomically decrements one chunk's RefCount, floored at 0.
func decrementRefCountTx(ctx context.Context, x storesql.Executor, id string) (uint32, error) {
	var newCount uint32
	err := x.QueryRow(ctx,
		`UPDATE file_blocks SET ref_count = GREATEST(ref_count - 1, 0) WHERE id = $1 RETURNING ref_count`,
		id).Scan(&newCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, metadata.ErrFileChunkNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("decrement ref count: %w", err)
	}
	return newCount, nil
}

// addRefTx bumps RefCount on the row(s) indexed by a content hash.
//
// Atomicity: a single UPDATE performs the bump — PostgreSQL's row-level locking
// serializes contended updates against the same row, so this is TOCTOU-free
// against a concurrent DecrementRefCount cascade.
//
// state = 2 (Remote) scoping mirrors GetByHash and the memory/badger backends,
// whose AddRef resolves a hash only through the finalized hash index. The dedup
// hit path references a block already confirmed on the remote; a Pending row
// (which now also carries its hash) is not a valid dedup donor, so this must
// miss it and return ErrUnknownHash, letting the caller fall back to full Put.
//
// The hash index on file_blocks is a NON-UNIQUE partial index (migration
// 000011), so one hash may match multiple rows in legacy data. The UPDATE
// deliberately omits LIMIT — all matching rows are bumped uniformly so refcount
// accounting stays correct regardless of which row a later decrement targets.
//
// Only ref_count is mutated; block_state is never touched.
func addRefTx(ctx context.Context, x storesql.Executor, hash block.ContentHash) error {
	result, err := x.Exec(ctx,
		`UPDATE file_blocks SET ref_count = ref_count + 1 WHERE hash = $1 AND state = 2 /* Remote */`,
		hash.String())
	if err != nil {
		return fmt.Errorf("add ref: %w", err)
	}
	if result.RowsAffected() == 0 {
		return metadata.ErrUnknownHash
	}
	return nil
}

// getByHashTx looks up a finalized chunk by content hash, returning (nil, nil)
// when absent. Dedup matches only Remote (state=2) chunks — Pending or Syncing
// rows have not been confirmed on the remote and are unsafe dedup targets.
func getByHashTx(ctx context.Context, x storesql.Executor, hash metadata.ContentHash) (*metadata.FileChunk, error) {
	chunk, err := scanFileChunk(x.QueryRow(ctx, selectFileChunkByHash, hash.String()))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find file chunk by hash: %w", err)
	}
	return chunk, nil
}

// listFileChunksTx returns the chunks belonging to one payload, in block order.
func listFileChunksTx(ctx context.Context, x storesql.Executor, payloadID string) ([]*metadata.FileChunk, error) {
	lo, hi := block.PayloadPrefixRange(payloadID)
	rows, err := x.Query(ctx, listFileChunksQuery, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("list file chunks: %w", err)
	}
	defer rows.Close()
	result, err := scanFileChunkRows(rows)
	if err != nil {
		return nil, err
	}
	return block.ChunksForPayload(result, payloadID), nil
}

// enumerateFileChunksTx streams every live-set ContentHash through fn, unioning
// the CAS index with the per-file manifest (see enumerateHashesQuery).
func enumerateFileChunksTx(ctx context.Context, x storesql.Executor, fn func(block.ContentHash) error) error {
	rows, err := x.Query(ctx, enumerateHashesQuery)
	if err != nil {
		return fmt.Errorf("enumerate file chunks: query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("enumerate file chunks: %w", err)
		}
		var hashStr sql.NullString
		if err := rows.Scan(&hashStr); err != nil {
			return fmt.Errorf("enumerate file chunks: scan: %w", err)
		}
		var h block.ContentHash
		if hashStr.Valid {
			parsed, perr := metadata.ParseContentHash(hashStr.String)
			if perr != nil {
				// Fail closed: a malformed hash row cannot be silently coerced
				// to the zero hash — that would invite the GC mark phase to
				// treat the row as a legacy pre-CAS entry and the sweep would
				// reap a still-live CAS object once the grace TTL lapses.
				// Surface the parse error so enumeration aborts and the sweep
				// is skipped.
				return fmt.Errorf("enumerate file chunks: parse hash %q: %w",
					hashStr.String, perr)
			}
			h = parsed
		}
		if err := fn(h); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("enumerate file chunks: rows: %w", err)
	}
	return nil
}

// AddRef atomically bumps RefCount on the FileChunk row(s) indexed by the
// given content hash, implementing the FileChunkStore.AddRef contract used by
// the in-memory hash dedup LRU hit path. Returns metadata.ErrUnknownHash when
// no row matches; callers fall back to the full Put path on that sentinel.
func (s *PostgresMetadataStore) AddRef(ctx context.Context, hash block.ContentHash, _ string, _ block.ChunkRef) error {
	// payloadID + blockRef are accepted for future GC traceability; this
	// backend records ref count only, so they are intentionally blanked.
	return addRefTx(ctx, s.conn(), hash)
}

// GetByHash looks up a finalized block by its content hash, returning
// (nil, nil) when absent.
func (s *PostgresMetadataStore) GetByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileChunk, error) {
	return getByHashTx(ctx, s.conn(), hash)
}

// listFileChunksQuery takes the bounds of block.PayloadPrefixRange and is a
// prefilter; block.ChunksForPayload decides membership and order.
//
// The range is compared and ordered in byte collation, not the database
// default: the bounds only bracket the prefix under byte ordering, and a
// byte-ordered id column lets the primary-key index seek the range instead of
// filtering the whole table.
const listFileChunksQuery = `SELECT ` + fileChunkColumns + `
	FROM file_blocks
	WHERE id >= $1 COLLATE "C" AND id < $2 COLLATE "C"
	ORDER BY id COLLATE "C" ASC`

// ListFileChunks returns all blocks belonging to a file, ordered by block index.
// Not on the narrowed FileChunkStore interface; kept as a backend method for
// engine-internal callers.
func (s *PostgresMetadataStore) ListFileChunks(ctx context.Context, payloadID string) ([]*metadata.FileChunk, error) {
	return listFileChunksTx(ctx, s.conn(), payloadID)
}

// enumerateHashesQuery is the GC mark live-set query. It UNIONs the CAS index
// (file_blocks.hash, VARCHAR hex) with the per-file manifest
// (file_block_refs.hash, BYTEA → encode hex) so the live set is a strict
// SUPERSET of both structures, so a hash present in only one (e.g. a manifest
// row whose CAS index row was never written or already reaped) still keeps its
// chunk live. The manifest arm is filtered to nlink>0 inodes (#1433): once a
// file is unlinked its manifest rows linger but the payload is dead, so
// including them would pin orphaned chunks live forever and the sweep could
// never reclaim them. Snapshot-held blocks are protected independently by the
// GC HoldProvider (on-disk snapshot manifests), not by this union. NULL
// hashes (legacy pre-CAS file_blocks rows) are emitted as the zero ContentHash
// and skipped by the mark phase; file_block_refs.hash is NOT NULL.
//
// UNION ALL, not UNION: the consumer (GC mark phase) dedupes hashes into a set,
// so cross-source and intra-source duplicates are harmless. UNION would force an
// expensive sort/hash-aggregate to dedupe at the query layer for no benefit.
const enumerateHashesQuery = `SELECT hash FROM file_blocks
UNION ALL
SELECT encode(fbr.hash, 'hex') FROM file_block_refs fbr
JOIN inodes i ON fbr.file_id = i.id
WHERE i.nlink > 0`

// EnumeratePayloads streams every distinct payloadID that has at least one
// FileChunk row through fn. FileChunk row IDs have the form
// {payloadID}/{chunkOffset}; the payloadID is everything BEFORE THE LAST '/'
// (payloadIDs are BuildPayloadID(shareName, filePath) and themselves contain
// slashes, so a split_part on the FIRST slash would truncate every
// subdirectory file to its share name). We therefore parse the payloadID in
// Go on the last slash rather than in SQL.
//
// The rows cursor is fully drained and CLOSED before any fn callback runs:
// fn may issue further reads, so we collect first then call — matching the
// sqlite/badger/memory backends. (The pgx pool is multi-connection, so this
// is for correctness/consistency rather than deadlock avoidance.)
func (s *PostgresMetadataStore) EnumeratePayloads(ctx context.Context, fn func(payloadID string) error) error {
	const query = `SELECT DISTINCT id FROM file_blocks`
	ids, err := s.collectFileChunkIDs(ctx, query)
	if err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("enumerate payloads: %w", err)
		}
		i := strings.LastIndex(id, "/")
		if i < 0 {
			continue
		}
		payloadID := id[:i]
		if _, ok := seen[payloadID]; ok {
			continue
		}
		seen[payloadID] = struct{}{}
		if err := fn(payloadID); err != nil {
			return err
		}
	}
	return nil
}

// EnumerateLivePayloadIDs streams every distinct content_id referenced by a
// live inode. content_id IS the payloadID. Hardlinks share one inode row, so
// DISTINCT yields one payloadID per content regardless of link count. nlink=0
// (unlinked) inodes are excluded (#1433): their payload is dead, so the
// reconcile must treat it as stranded, not live.
func (s *PostgresMetadataStore) EnumerateLivePayloadIDs(ctx context.Context, fn func(payloadID string) error) error {
	const query = `SELECT DISTINCT content_id FROM inodes WHERE content_id IS NOT NULL AND content_id != '' AND nlink > 0`
	ids, err := s.collectFileChunkIDs(ctx, query)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("enumerate live payloads: %w", err)
		}
		if err := fn(id); err != nil {
			return err
		}
	}
	return nil
}

// collectFileChunkIDs runs query (which must SELECT a single TEXT id column),
// scans every id into a slice, and closes the rows cursor before returning.
func (s *PostgresMetadataStore) collectFileChunkIDs(ctx context.Context, query string) ([]string, error) {
	rows, err := s.query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("enumerate payloads: query: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("enumerate payloads: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("enumerate payloads: rows: %w", err)
	}
	return ids, nil
}

// EnumerateFileChunks streams every live-set ContentHash through fn.
func (s *PostgresMetadataStore) EnumerateFileChunks(ctx context.Context, fn func(block.ContentHash) error) error {
	return enumerateFileChunksTx(ctx, s.conn(), fn)
}

// ============================================================================
// Scan Helpers
// ============================================================================

// scanFileChunk scans a single row into a FileChunk.
func scanFileChunk(row storesql.Row) (*metadata.FileChunk, error) {
	var (
		block             metadata.FileChunk
		hashStr           sql.NullString
		lastSyncAttemptAt sql.NullTime
	)
	if err := row.Scan(&block.ID, &hashStr, &block.DataSize, &block.StartOffset,
		&block.RefCount, &block.LastAccess, &block.CreatedAt, &block.State, &lastSyncAttemptAt); err != nil {
		return nil, err
	}
	if hashStr.Valid {
		// do not silently coerce malformed CAS hashes to the
		// zero hash — see EnumerateFileChunks for the data-loss scenario.
		h, perr := metadata.ParseContentHash(hashStr.String)
		if perr != nil {
			return nil, fmt.Errorf("scan file chunk %s: parse hash %q: %w",
				block.ID, hashStr.String, perr)
		}
		block.Hash = h
	}
	if lastSyncAttemptAt.Valid {
		block.LastSyncAttemptAt = lastSyncAttemptAt.Time
	}
	return &block, nil
}

// scanFileChunkRows scans multiple rows into FileChunk slices.
func scanFileChunkRows(rows storesql.Rows) ([]*metadata.FileChunk, error) {
	var result []*metadata.FileChunk
	for rows.Next() {
		block, err := scanFileChunk(rows)
		if err != nil {
			return nil, fmt.Errorf("scan file chunk: %w", err)
		}
		result = append(result, block)
	}
	return result, rows.Err()
}

// ============================================================================
// Transaction Support
// ============================================================================

// Ensure postgresTransaction implements FileChunkStore
var _ block.FileChunkStore = (*postgresTransaction)(nil)

// The FileChunkStore methods on
// postgresTransaction MUST execute against the txn's own pgx.Tx, not the
// public store's connection-pool helpers. Previously every method just
// called `tx.store.X(...)` which routed through the pool — defeating
// rollback for any caller that bumped RefCount inside WithTransaction
// then encountered a downstream UpdateAttrs failure (silent
// leak). All proxies below are now tx-bound; non-mutating
// helpers keep the pool path because no caller mutates state through them.

func (tx *postgresTransaction) GetFileChunk(ctx context.Context, id string) (*metadata.FileChunk, error) {
	return getFileChunkTx(ctx, tx.conn(), id)
}

func (tx *postgresTransaction) Put(ctx context.Context, chunk *metadata.FileChunk) error {
	return putFileChunkTx(ctx, tx.conn(), chunk)
}

func (tx *postgresTransaction) Delete(ctx context.Context, id string) error {
	return deleteFileChunkTx(ctx, tx.conn(), id)
}

// The ref-count mutators below run on the active pgx.Tx so a subsequent
// rollback undoes them. Production callers reach them through
// metadataCoordinator when ctx carries an active tx via metadata.WithTx.

func (tx *postgresTransaction) IncrementRefCount(ctx context.Context, id string) error {
	return incrementRefCountTx(ctx, tx.conn(), id)
}

func (tx *postgresTransaction) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	return decrementRefCountTx(ctx, tx.conn(), id)
}

// DecrementRefCountAndReap runs the -1 UPDATE + reap-at-zero DELETE on the
// active transaction so a rollback undoes both. Returns (0, nil) when the row is
// already absent.
func (tx *postgresTransaction) DecrementRefCountAndReap(ctx context.Context, id string) (uint32, error) {
	return decrementAndReapTx(ctx, tx.conn(), id)
}

// DecrementRefCountAndReapMany is the batched form, applied to the whole id set
// in two statements on the active transaction.
func (tx *postgresTransaction) DecrementRefCountAndReapMany(ctx context.Context, ids []string) error {
	return decrementAndReapManyTx(ctx, tx.conn(), ids)
}

// AddRef bumps the ref count keyed by hash on the active transaction, giving
// the LRU hit path rollback parity. Returns metadata.ErrUnknownHash on no match.
func (tx *postgresTransaction) AddRef(ctx context.Context, hash block.ContentHash, _ string, _ block.ChunkRef) error {
	// payloadID + blockRef accepted for future GC traceability; intentionally
	// blanked, as on the pool path.
	return addRefTx(ctx, tx.conn(), hash)
}

func (tx *postgresTransaction) GetByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileChunk, error) {
	return getByHashTx(ctx, tx.conn(), hash)
}

// ListFileChunks and EnumerateFileChunks run on the active transaction, NOT the
// pool. Going through the pool would open a separate connection that cannot see
// this transaction's uncommitted writes, so a Put followed by a List inside one
// WithTransaction would miss the pending row — a read-after-write violation.
// That is the whole reason these take an Executor rather than being called on
// the store.

func (tx *postgresTransaction) ListFileChunks(ctx context.Context, payloadID string) ([]*metadata.FileChunk, error) {
	return listFileChunksTx(ctx, tx.conn(), payloadID)
}

func (tx *postgresTransaction) EnumerateFileChunks(ctx context.Context, fn func(block.ContentHash) error) error {
	return enumerateFileChunksTx(ctx, tx.conn(), fn)
}

// The file_blocks table schema lives in
// pkg/metadata/store/postgres/migrations/000010_file_blocks.up.sql.

// InjectCorruptHashRow stores a file_blocks row whose hash column holds a
// syntactically malformed value. Test-only: implements the storetest
// CorruptHashInjector capability so the conformance suite can exercise
// fail-closed enumeration. The TEXT column lets us bypass the
// Put contract that always serializes a valid ContentHash.String().
func (s *PostgresMetadataStore) InjectCorruptHashRow(ctx context.Context, blockID string, badHash string) error {
	now := time.Now()
	_, err := s.exec(ctx, insertFileChunk+` ON CONFLICT (id) DO UPDATE SET hash = EXCLUDED.hash`,
		blockID, badHash, uint32(64), uint32(0), uint32(1), now, now, int(block.BlockStateRemote), nil,
	)
	if err != nil {
		return fmt.Errorf("inject corrupt hash row: %w", err)
	}
	return nil
}
