package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// FileChunkStore Implementation for SQLite Store
// ============================================================================
//
// This file implements the FileChunkStore interface for the SQLite metadata store.
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
// Thread Safety: All operations use SQLite transactions for ACID guarantees.
//
// ============================================================================

// Ensure SQLiteMetadataStore implements FileChunkStore
var _ block.FileChunkStore = (*SQLiteMetadataStore)(nil)

// ============================================================================
// FileChunk Operations
// ============================================================================

// GetFileChunk retrieves a file chunk by its ID. Not on the narrowed
// FileChunkStore interface; kept as a backend
// method for engine-internal callers.
func (s *SQLiteMetadataStore) GetFileChunk(ctx context.Context, id string) (*metadata.FileChunk, error) {
	query := `SELECT id, hash, data_size, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at
		FROM file_blocks WHERE id = ?1`
	row := s.queryRow(ctx, query, id)

	block, err := scanFileChunk(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, metadata.ErrFileChunkNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get file chunk: %w", err)
	}
	return block, nil
}

// Put stores or updates a file chunk.
//
// The hash column is persisted whenever the block carries a non-zero
// content hash, regardless of block state. The content hash is derived
// at rollup time (long before the block reaches the remote) and is the
// key the engine's CAS read path uses to resolve a chunk. Gating the
// write on IsFinalized() left every Pending row with a NULL hash; reads
// then survived only while the bytes stayed in the local append log or
// RAM cache, and broke the moment local state went cold (restart +
// cache eviction, or a snapshot restore's ResetLocalState). The memory
// and badger backends always store the hash inline on the row, so this
// matches their behavior.
func (s *SQLiteMetadataStore) Put(ctx context.Context, block *metadata.FileChunk) error {
	var hashStr *string
	if !block.Hash.IsZero() {
		h := block.Hash.String()
		hashStr = &h
	}
	var blockStoreKey *string
	if block.BlockStoreKey != "" {
		blockStoreKey = &block.BlockStoreKey
	}
	// persist LastSyncAttemptAt as NULL when zero so the
	// janitor's WHERE last_sync_attempt_at < cutoff predicate excludes
	// never-claimed rows naturally instead of matching every Pending row.
	var lastSyncAttemptAt *time.Time
	if !block.LastSyncAttemptAt.IsZero() {
		t := block.LastSyncAttemptAt
		lastSyncAttemptAt = &t
	}

	// Omit ref_count from the ON CONFLICT UPDATE list so concurrent
	// IncrementRefCount / DecrementRefCount (which run as atomic SQL `+1` /
	// `-1` UPDATEs) cannot be silently overwritten by a stale
	// Put-with-in-memory-RefCount. RefCount on the INSERT path is still set
	// verbatim from the caller's *FileChunk (matches the contract for new
	// rows). For existing rows, RefCount mutates exclusively through
	// Increment/Decrement. hash uses COALESCE so a zero-hash Put never NULLs
	// a previously-persisted good hash.
	query := `
		INSERT INTO file_blocks (id, hash, data_size, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)
		ON CONFLICT (id) DO UPDATE SET
			hash = COALESCE(EXCLUDED.hash, file_blocks.hash),
			data_size = EXCLUDED.data_size,
			block_store_key = EXCLUDED.block_store_key,
			last_access = EXCLUDED.last_access,
			state = EXCLUDED.state,
			last_sync_attempt_at = EXCLUDED.last_sync_attempt_at`
	_, err := s.exec(ctx, query,
		block.ID, hashStr, block.DataSize, blockStoreKey,
		block.RefCount, block.LastAccess, block.CreatedAt, block.State, lastSyncAttemptAt)
	if err != nil {
		return fmt.Errorf("put file chunk: %w", err)
	}
	return nil
}

// Delete removes a file chunk by its ID. Renamed from DeleteFileChunk in
func (s *SQLiteMetadataStore) Delete(ctx context.Context, id string) error {
	result, err := s.exec(ctx, `DELETE FROM file_blocks WHERE id = ?1`, id)
	if err != nil {
		return fmt.Errorf("delete file chunk: %w", err)
	}
	rows := result.RowsAffected()
	if rows == 0 {
		return metadata.ErrFileChunkNotFound
	}
	return nil
}

// IncrementRefCount atomically increments a block's RefCount.
func (s *SQLiteMetadataStore) IncrementRefCount(ctx context.Context, id string) error {
	result, err := s.exec(ctx,
		`UPDATE file_blocks SET ref_count = ref_count + 1 WHERE id = ?1`, id)
	if err != nil {
		return fmt.Errorf("increment ref count: %w", err)
	}
	rows := result.RowsAffected()
	if rows == 0 {
		return metadata.ErrFileChunkNotFound
	}
	return nil
}

// DecrementRefCount atomically decrements a block's RefCount.
func (s *SQLiteMetadataStore) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	query := `UPDATE file_blocks SET ref_count = MAX(ref_count - 1, 0) WHERE id = ?1 RETURNING ref_count`
	var newCount uint32
	err := s.queryRow(ctx, query, id).Scan(&newCount)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, metadata.ErrFileChunkNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("decrement ref count: %w", err)
	}
	return newCount, nil
}

// DecrementRefCountAndReap atomically decrements ref_count and, when it hits 0,
// deletes the row — both statements run inside ONE transaction so the
// decrement-and-reap is atomic and TOCTOU-free against a concurrent AddRef
// (which takes the same row lock). The conditional `ref_count = 0` predicate on
// the DELETE means a concurrent bump that landed between the two statements
// (impossible within the row lock, but defended anyway) leaves the row alive.
// Returns (0, nil) when the row is already absent — a swept row is not a caller
// error.
func (s *SQLiteMetadataStore) DecrementRefCountAndReap(ctx context.Context, id string) (uint32, error) {
	rawTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("decrement-and-reap begin tx: %w", err)
	}
	defer func() { _ = rawTx.Rollback() }()

	newCount, err := decrementAndReapTx(ctx, execer{e: rawTx, op: "DecrementRefCountAndReap"}, id)
	if err != nil {
		return 0, err
	}
	if err := rawTx.Commit(); err != nil {
		return 0, fmt.Errorf("decrement-and-reap commit: %w", err)
	}
	return newCount, nil
}

// decrementAndReapTx runs the -1 UPDATE then a conditional DELETE on the given
// pgx.Tx. Shared by the pool-backed store method and the sqliteTransaction
// method so both surfaces behave identically. Returns ErrFileChunkNotFound
// mapped to (0, nil) — i.e. a missing row is tolerated.
func decrementAndReapTx(ctx context.Context, tx execer, id string) (uint32, error) {
	var newCount uint32
	err := tx.QueryRow(ctx,
		`UPDATE file_blocks SET ref_count = MAX(ref_count - 1, 0) WHERE id = ?1 RETURNING ref_count`,
		id).Scan(&newCount)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil // tolerate already-swept row
	}
	if err != nil {
		return 0, fmt.Errorf("decrement ref count: %w", err)
	}
	if newCount == 0 {
		if _, err := tx.Exec(ctx,
			`DELETE FROM file_blocks WHERE id = ?1 AND ref_count = 0`, id); err != nil {
			return 0, fmt.Errorf("reap zero-ref block: %w", err)
		}
	}
	return newCount, nil
}

// AddRef atomically bumps RefCount on the FileChunk row(s) indexed by
// the given content hash. Implements the FileChunkStore.AddRef contract
// used by the in-memory hash dedup LRU hit path to
// reference an already-stored block without creating a new row.
//
// Atomicity: a single UPDATE statement performs the bump — the SQLite
// single-writer engine serializes contended updates against the same row,
// so AddRef is TOCTOU-free against concurrent DecrementRefCount cascade
// (matches the existing IncrementRefCount idiom).
//
// Returns metadata.ErrUnknownHash when RowsAffected == 0 (no row exists
// for this hash). Callers (the LRU hit site) fall back to the full Put
// path on this sentinel.
//
// Multi-row-per-hash tolerance:
// the hash index on file_blocks is a NON-UNIQUE partial index (migration
// 000011), so a single hash may match multiple rows in legacy data. The
// UPDATE deliberately omits LIMIT — all matching rows are bumped
// uniformly so refcount accounting stays correct regardless of which
// row a later DecrementRefCount targets. The conformance test seeds a
// single row, so RefCount goes from N to N+1 exactly.
//
// Only ref_count is mutated. block_state is never touched: AddRef
// references an existing block, and the LRU hit path never creates
// or transitions one.
func (s *SQLiteMetadataStore) AddRef(ctx context.Context, hash block.ContentHash, _ string, _ block.ChunkRef) error {
	// payloadID + blockRef accepted for future GC traceability;
	// postgres backend records ref count only — parameters intentionally
	// blanked.
	//
	// state = 2 (Remote) scoping mirrors GetByHash and the memory/badger
	// backends, whose AddRef resolves the hash only through the finalized
	// hash index. The dedup hit path references a block already confirmed
	// on the remote; a Pending row (which now also carries its hash) is
	// not a valid dedup donor, so AddRef must miss it and return
	// ErrUnknownHash exactly as before, letting the caller fall back to
	// the full Put path.
	result, err := s.exec(ctx,
		`UPDATE file_blocks SET ref_count = ref_count + 1 WHERE hash = ?1 AND state = 2 /* Remote */`,
		hash.String())
	if err != nil {
		return fmt.Errorf("add ref: %w", err)
	}
	if result.RowsAffected() == 0 {
		return metadata.ErrUnknownHash
	}
	return nil
}

// GetByHash looks up a finalized block by its content hash.
// Returns nil without error if not found. Renamed from
// FindFileChunkByHash. Dedup matches only Remote (state=2) blocks —
// Pending or Syncing rows have not been confirmed on the remote and
// are unsafe dedup targets.
func (s *SQLiteMetadataStore) GetByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileChunk, error) {
	query := `SELECT id, hash, data_size, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at
		FROM file_blocks WHERE hash = ?1 AND state = 2 /* Remote */`
	row := s.queryRow(ctx, query, hash.String())

	block, err := scanFileChunk(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find file chunk by hash: %w", err)
	}
	return block, nil
}

// ListFileChunks returns all blocks belonging to a file, ordered by block index.
// Uses LIKE query on block ID prefix, then sorts in Go for correct numeric ordering.
// Not on the narrowed FileChunkStore interface;
// kept as a backend method for engine-internal callers.
func (s *SQLiteMetadataStore) ListFileChunks(ctx context.Context, payloadID string) ([]*metadata.FileChunk, error) {
	query := `SELECT id, hash, data_size, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at
		FROM file_blocks
		WHERE id LIKE ?1
		ORDER BY id ASC`
	rows, err := s.query(ctx, query, payloadID+"/%")
	if err != nil {
		return nil, fmt.Errorf("list file chunks: %w", err)
	}
	defer rows.Close()
	result, err := scanFileChunkRows(rows)
	if err != nil {
		return nil, err
	}
	// SQL ORDER BY id ASC gives lexicographic order which is wrong for multi-digit
	// block indices (e.g., "10" < "2"). Sort by parsed numeric index.
	sort.Slice(result, func(i, j int) bool {
		return parseBlockIdx(result[i].ID) < parseBlockIdx(result[j].ID)
	})
	if result == nil {
		return []*metadata.FileChunk{}, nil
	}
	return result, nil
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
SELECT lower(hex(fbr.hash)) FROM file_block_refs fbr
JOIN inodes i ON fbr.file_id = i.id
WHERE i.nlink > 0`

// EnumeratePayloads streams every distinct payloadID that has at least one
// FileChunk row through fn. FileChunk row IDs have the form
// {payloadID}/{chunkOffset}; the payloadID is everything BEFORE THE LAST '/'
// (payloadIDs are BuildPayloadID(shareName, filePath) and themselves contain
// slashes, so a substr/split on the FIRST slash would truncate every
// subdirectory file to its share name). We therefore parse the payloadID in
// Go on the last slash rather than in SQL.
//
// The rows cursor is fully drained and CLOSED before any fn callback runs.
// The SQLite pool is MaxOpenConns(1), and warm/stats fn callbacks issue
// further reads (ListFileChunks) that need that single connection — calling fn
// while the cursor is still open would deadlock. This collect-then-call shape
// also matches the badger/memory backends.
func (s *SQLiteMetadataStore) EnumeratePayloads(ctx context.Context, fn func(payloadID string) error) error {
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
// live inode. content_id IS the payloadID, so no id-splitting is needed.
// Hardlinks share one inode row, so DISTINCT yields one payloadID regardless of
// link count. nlink=0 (unlinked) inodes are excluded (#1433): their payload is
// dead, so the reconcile must treat it as stranded, not live.
func (s *SQLiteMetadataStore) EnumerateLivePayloadIDs(ctx context.Context, fn func(payloadID string) error) error {
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
// scans every id into a slice, and closes the rows cursor before returning so
// the caller may safely issue further queries on the single-connection pool.
func (s *SQLiteMetadataStore) collectFileChunkIDs(ctx context.Context, query string) ([]string, error) {
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

// EnumerateFileChunks streams every live-set ContentHash through fn, unioning
// the CAS index with the per-file manifest (see enumerateHashesQuery).
func (s *SQLiteMetadataStore) EnumerateFileChunks(ctx context.Context, fn func(block.ContentHash) error) error {
	rows, err := s.query(ctx, enumerateHashesQuery)
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
				// (mark fail-closed): a malformed hash row
				// cannot be silently coerced to the zero hash — that would
				// invite the GC mark phase to treat the row as a legacy
				// pre-CAS entry and the sweep would reap a still-live CAS
				// object once the grace TTL lapses. Surface the parse error
				// so EnumerateFileChunks aborts and the sweep is skipped.
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

// parseBlockIdx returns the numeric suffix of a block ID ("{payloadID}/{n}"), used as a sort key; 0 if absent.
func parseBlockIdx(id string) int {
	if idx := strings.LastIndex(id, "/"); idx >= 0 {
		if v, err := strconv.Atoi(id[idx+1:]); err == nil {
			return v
		}
	}
	return 0
}

// ============================================================================
// Scan Helpers
// ============================================================================

// scanFileChunk scans a single row into a FileChunk.
func scanFileChunk(row scanRow) (*metadata.FileChunk, error) {
	var (
		block             metadata.FileChunk
		hashStr           sql.NullString
		blockStoreKey     sql.NullString
		lastSyncAttemptAt sql.NullTime
	)
	if err := row.Scan(&block.ID, &hashStr, &block.DataSize, &blockStoreKey,
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
	if blockStoreKey.Valid {
		block.BlockStoreKey = blockStoreKey.String
	}
	if lastSyncAttemptAt.Valid {
		block.LastSyncAttemptAt = lastSyncAttemptAt.Time
	}
	return &block, nil
}

// scanFileChunkRows scans multiple rows into FileChunk slices.
func scanFileChunkRows(rows scanRows) ([]*metadata.FileChunk, error) {
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

// Ensure sqliteTransaction implements FileChunkStore
var _ block.FileChunkStore = (*sqliteTransaction)(nil)

// The FileChunkStore methods on
// sqliteTransaction MUST execute against the txn's own pgx.Tx, not the
// public store's connection-pool helpers. Previously every method just
// called `tx.store.X(...)` which routed through the pool — defeating
// rollback for any caller that bumped RefCount inside WithTransaction
// then encountered a downstream PutFile failure (silent
// leak). All proxies below are now tx-bound; non-mutating
// helpers keep the pool path because no caller mutates state through them.

func (tx *sqliteTransaction) GetFileChunk(ctx context.Context, id string) (*metadata.FileChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	query := `SELECT id, hash, data_size, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at
		FROM file_blocks WHERE id = ?1`
	row := tx.tx.QueryRow(ctx, query, id)
	block, err := scanFileChunk(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, metadata.ErrFileChunkNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get file chunk: %w", err)
	}
	return block, nil
}

func (tx *sqliteTransaction) Put(ctx context.Context, block *metadata.FileChunk) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var hashStr *string
	if !block.Hash.IsZero() {
		h := block.Hash.String()
		hashStr = &h
	}
	var blockStoreKey *string
	if block.BlockStoreKey != "" {
		blockStoreKey = &block.BlockStoreKey
	}
	var lastSyncAttemptAt *time.Time
	if !block.LastSyncAttemptAt.IsZero() {
		t := block.LastSyncAttemptAt
		lastSyncAttemptAt = &t
	}
	// Omit ref_count from the ON CONFLICT update list (matches the pool-path
	// Put). RefCount mutates only via Increment/Decrement. hash uses COALESCE
	// so a zero-hash Put never NULLs a previously-persisted good hash.
	query := `
		INSERT INTO file_blocks (id, hash, data_size, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)
		ON CONFLICT (id) DO UPDATE SET
			hash = COALESCE(EXCLUDED.hash, file_blocks.hash),
			data_size = EXCLUDED.data_size,
			block_store_key = EXCLUDED.block_store_key,
			last_access = EXCLUDED.last_access,
			state = EXCLUDED.state,
			last_sync_attempt_at = EXCLUDED.last_sync_attempt_at`
	_, err := tx.tx.Exec(ctx, query,
		block.ID, hashStr, block.DataSize, blockStoreKey,
		block.RefCount, block.LastAccess, block.CreatedAt, block.State, lastSyncAttemptAt)
	if err != nil {
		return fmt.Errorf("put file chunk: %w", err)
	}
	return nil
}

func (tx *sqliteTransaction) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := tx.tx.Exec(ctx, `DELETE FROM file_blocks WHERE id = ?1`, id)
	if err != nil {
		return fmt.Errorf("delete file chunk: %w", err)
	}
	if result.RowsAffected() == 0 {
		return metadata.ErrFileChunkNotFound
	}
	return nil
}

// IncrementRefCount runs the +1 UPDATE on the active pgx.Tx so a
// subsequent rollback undoes the bump (fix). Production callers
// route here through metadataCoordinator.IncrementRefCount when ctx
// carries an active tx via metadata.WithTx.
func (tx *sqliteTransaction) IncrementRefCount(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := tx.tx.Exec(ctx,
		`UPDATE file_blocks SET ref_count = ref_count + 1 WHERE id = ?1`, id)
	if err != nil {
		return fmt.Errorf("increment ref count: %w", err)
	}
	if result.RowsAffected() == 0 {
		return metadata.ErrFileChunkNotFound
	}
	return nil
}

// DecrementRefCount runs the -1 UPDATE on the active pgx.Tx so a
// subsequent rollback undoes the decrement (fix).
func (tx *sqliteTransaction) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	query := `UPDATE file_blocks SET ref_count = MAX(ref_count - 1, 0) WHERE id = ?1 RETURNING ref_count`
	var newCount uint32
	err := tx.tx.QueryRow(ctx, query, id).Scan(&newCount)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, metadata.ErrFileChunkNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("decrement ref count: %w", err)
	}
	return newCount, nil
}

// DecrementRefCountAndReap runs the -1 UPDATE + reap-at-zero DELETE on the
// active pgx.Tx so a subsequent rollback undoes both. Returns (0, nil) when the
// row is already absent.
func (tx *sqliteTransaction) DecrementRefCountAndReap(ctx context.Context, id string) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return decrementAndReapTx(ctx, tx.tx, id)
}

// AddRef runs the +1 UPDATE keyed by hash on the active pgx.Tx so a
// subsequent rollback undoes the bump (parity for the
// LRU hit path). Returns metadata.ErrUnknownHash when no row matches.
func (tx *sqliteTransaction) AddRef(ctx context.Context, hash block.ContentHash, _ string, _ block.ChunkRef) error {
	// payloadID + blockRef accepted for future GC traceability;
	// postgres backend records ref count only — parameters intentionally
	// blanked.
	if err := ctx.Err(); err != nil {
		return err
	}
	// state = 2 (Remote) scoping mirrors the pool-path AddRef and the
	// memory/badger backends — a Pending row is not a valid dedup donor.
	result, err := tx.tx.Exec(ctx,
		`UPDATE file_blocks SET ref_count = ref_count + 1 WHERE hash = ?1 AND state = 2 /* Remote */`,
		hash.String())
	if err != nil {
		return fmt.Errorf("add ref: %w", err)
	}
	if result.RowsAffected() == 0 {
		return metadata.ErrUnknownHash
	}
	return nil
}

func (tx *sqliteTransaction) GetByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	query := `SELECT id, hash, data_size, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at
		FROM file_blocks WHERE hash = ?1 AND state = 2 /* Remote */`
	row := tx.tx.QueryRow(ctx, query, hash.String())
	block, err := scanFileChunk(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find file chunk by hash: %w", err)
	}
	return block, nil
}

// ListFileChunks / EnumerateFileChunks run on the active
// transaction (tx.tx), NOT the pool. Delegating to the pool opens a separate
// connection that cannot see this transaction's uncommitted writes, so a Put
// followed by a List in the same WithTransaction would miss the pending row
// (read-after-write violation; the SQL is otherwise identical to the
// store-level methods).

func (tx *sqliteTransaction) ListFileChunks(ctx context.Context, payloadID string) ([]*metadata.FileChunk, error) {
	query := `SELECT id, hash, data_size, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at
		FROM file_blocks
		WHERE id LIKE ?1
		ORDER BY id ASC`
	rows, err := tx.tx.Query(ctx, query, payloadID+"/%")
	if err != nil {
		return nil, fmt.Errorf("list file chunks: %w", err)
	}
	defer rows.Close()
	result, err := scanFileChunkRows(rows)
	if err != nil {
		return nil, err
	}
	// Lexicographic SQL order mis-sorts multi-digit indices ("10" < "2");
	// sort by parsed numeric index, matching the store-level method.
	sort.Slice(result, func(i, j int) bool {
		return parseBlockIdx(result[i].ID) < parseBlockIdx(result[j].ID)
	})
	if result == nil {
		return []*metadata.FileChunk{}, nil
	}
	return result, nil
}

func (tx *sqliteTransaction) EnumerateFileChunks(ctx context.Context, fn func(block.ContentHash) error) error {
	rows, err := tx.tx.Query(ctx, enumerateHashesQuery)
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
				return fmt.Errorf("enumerate file chunks: parse hash %q: %w", hashStr.String, perr)
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

// The file_blocks table schema lives in
// pkg/metadata/store/postgres/migrations/000010_file_blocks.up.sql.

// InjectCorruptHashRow stores a file_blocks row whose hash column holds a
// syntactically malformed value. Test-only: implements the storetest
// CorruptHashInjector capability so the conformance suite can exercise
// fail-closed enumeration. The TEXT column lets us bypass the
// Put contract that always serializes a valid ContentHash.String().
func (s *SQLiteMetadataStore) InjectCorruptHashRow(ctx context.Context, blockID string, badHash string) error {
	now := time.Now()
	_, err := s.exec(ctx, `
		INSERT INTO file_blocks (id, hash, data_size, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)
		ON CONFLICT (id) DO UPDATE SET hash = EXCLUDED.hash`,
		blockID, badHash, uint32(64), nil, uint32(1), now, now, int(block.BlockStateRemote), nil,
	)
	if err != nil {
		return fmt.Errorf("inject corrupt hash row: %w", err)
	}
	return nil
}
