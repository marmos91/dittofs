package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// FileBlockStore Implementation for PostgreSQL Store
// ============================================================================
//
// This file implements the FileBlockStore interface for the PostgreSQL metadata store.
// It provides content-addressed file block tracking for deduplication and caching.
//
// The FileBlockStore interface is narrowed to 6 methods. The backend
// retains the legacy GetFileBlock + ListFileBlocks helpers as
// concrete methods on the struct (not on the public interface) for
// engine-internal callers.
//
// Table:
//   - file_blocks: File block data with UUID as primary key and hash index
//
// Thread Safety: All operations use PostgreSQL transactions for ACID guarantees.
//
// ============================================================================

// Ensure PostgresMetadataStore implements FileBlockStore
var _ blockstore.FileBlockStore = (*PostgresMetadataStore)(nil)

// ============================================================================
// FileBlock Operations
// ============================================================================

// GetFileBlock retrieves a file block by its ID. Not on the narrowed
// FileBlockStore interface; kept as a backend
// method for engine-internal callers.
func (s *PostgresMetadataStore) GetFileBlock(ctx context.Context, id string) (*metadata.FileBlock, error) {
	query := `SELECT id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at
		FROM file_blocks WHERE id = $1`
	row := s.queryRow(ctx, query, id)

	block, err := scanFileBlock(row)
	if err == pgx.ErrNoRows {
		return nil, metadata.ErrFileBlockNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get file block: %w", err)
	}
	return block, nil
}

// Put stores or updates a file block.
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
func (s *PostgresMetadataStore) Put(ctx context.Context, block *metadata.FileBlock) error {
	var hashStr *string
	if !block.Hash.IsZero() {
		h := block.Hash.String()
		hashStr = &h
	}
	var blockStoreKey *string
	if block.BlockStoreKey != "" {
		blockStoreKey = &block.BlockStoreKey
	}
	var cachePath *string
	if block.LocalPath != "" {
		cachePath = &block.LocalPath
	}
	// persist LastSyncAttemptAt as NULL when zero so the
	// janitor's WHERE last_sync_attempt_at < cutoff predicate excludes
	// never-claimed rows naturally instead of matching every Pending row.
	var lastSyncAttemptAt *time.Time
	if !block.LastSyncAttemptAt.IsZero() {
		t := block.LastSyncAttemptAt
		lastSyncAttemptAt = &t
	}

	// (review iteration 1): omit ref_count from the ON
	// CONFLICT UPDATE list so concurrent IncrementRefCount / DecrementRefCount
	// (which run as atomic SQL `+1` / `-1` UPDATEs) cannot be silently
	// overwritten by a stale Put-with-in-memory-RefCount. RefCount on the
	// INSERT path is still set verbatim from the caller's *FileBlock
	// (matches the contract for new rows). For existing rows, RefCount
	// mutates exclusively through Increment/Decrement.
	query := `
		INSERT INTO file_blocks (id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (id) DO UPDATE SET
			hash = EXCLUDED.hash,
			data_size = EXCLUDED.data_size,
			cache_path = EXCLUDED.cache_path,
			block_store_key = EXCLUDED.block_store_key,
			last_access = EXCLUDED.last_access,
			state = EXCLUDED.state,
			last_sync_attempt_at = EXCLUDED.last_sync_attempt_at`
	_, err := s.exec(ctx, query,
		block.ID, hashStr, block.DataSize, cachePath, blockStoreKey,
		block.RefCount, block.LastAccess, block.CreatedAt, block.State, lastSyncAttemptAt)
	if err != nil {
		return fmt.Errorf("put file block: %w", err)
	}
	return nil
}

// Delete removes a file block by its ID. Renamed from DeleteFileBlock in
func (s *PostgresMetadataStore) Delete(ctx context.Context, id string) error {
	result, err := s.exec(ctx, `DELETE FROM file_blocks WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete file block: %w", err)
	}
	rows := result.RowsAffected()
	if rows == 0 {
		return metadata.ErrFileBlockNotFound
	}
	return nil
}

// IncrementRefCount atomically increments a block's RefCount.
func (s *PostgresMetadataStore) IncrementRefCount(ctx context.Context, id string) error {
	result, err := s.exec(ctx,
		`UPDATE file_blocks SET ref_count = ref_count + 1 WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("increment ref count: %w", err)
	}
	rows := result.RowsAffected()
	if rows == 0 {
		return metadata.ErrFileBlockNotFound
	}
	return nil
}

// DecrementRefCount atomically decrements a block's RefCount.
func (s *PostgresMetadataStore) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	query := `UPDATE file_blocks SET ref_count = GREATEST(ref_count - 1, 0) WHERE id = $1 RETURNING ref_count`
	var newCount uint32
	err := s.queryRow(ctx, query, id).Scan(&newCount)
	if err == pgx.ErrNoRows {
		return 0, metadata.ErrFileBlockNotFound
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
func (s *PostgresMetadataStore) DecrementRefCountAndReap(ctx context.Context, id string) (uint32, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return 0, fmt.Errorf("decrement-and-reap begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	newCount, err := decrementAndReapTx(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("decrement-and-reap commit: %w", err)
	}
	return newCount, nil
}

// decrementAndReapTx runs the -1 UPDATE then a conditional DELETE on the given
// pgx.Tx. Shared by the pool-backed store method and the postgresTransaction
// method so both surfaces behave identically. Returns ErrFileBlockNotFound
// mapped to (0, nil) — i.e. a missing row is tolerated.
func decrementAndReapTx(ctx context.Context, tx pgx.Tx, id string) (uint32, error) {
	var newCount uint32
	err := tx.QueryRow(ctx,
		`UPDATE file_blocks SET ref_count = GREATEST(ref_count - 1, 0) WHERE id = $1 RETURNING ref_count`,
		id).Scan(&newCount)
	if err == pgx.ErrNoRows {
		return 0, nil // tolerate already-swept row
	}
	if err != nil {
		return 0, fmt.Errorf("decrement ref count: %w", err)
	}
	if newCount == 0 {
		if _, err := tx.Exec(ctx,
			`DELETE FROM file_blocks WHERE id = $1 AND ref_count = 0`, id); err != nil {
			return 0, fmt.Errorf("reap zero-ref block: %w", err)
		}
	}
	return newCount, nil
}

// AddRef atomically bumps RefCount on the FileBlock row(s) indexed by
// the given content hash. Implements the FileBlockStore.AddRef contract
// used by the in-memory hash dedup LRU hit path to
// reference an already-stored block without creating a new row.
//
// Atomicity: a single UPDATE statement performs the bump — PostgreSQL's
// row-level locking serializes contended updates against the same row,
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
func (s *PostgresMetadataStore) AddRef(ctx context.Context, hash blockstore.ContentHash, _ string, _ blockstore.BlockRef) error {
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

// GetByHash looks up a finalized block by its content hash.
// Returns nil without error if not found. Renamed from
// FindFileBlockByHash. Dedup matches only Remote (state=2) blocks —
// Pending or Syncing rows have not been confirmed on the remote and
// are unsafe dedup targets.
func (s *PostgresMetadataStore) GetByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileBlock, error) {
	query := `SELECT id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at
		FROM file_blocks WHERE hash = $1 AND state = 2 /* Remote */`
	row := s.queryRow(ctx, query, hash.String())

	block, err := scanFileBlock(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find file block by hash: %w", err)
	}
	return block, nil
}

// ListPending returns blocks in Pending state (RefCount>=1, not yet
// uploaded) older than the given duration. Renamed from
// ListLocalBlocks; the underlying semantics already match ("Local"
// was renamed Pending). If limit > 0, at most limit blocks are
// returned.
//
// The legacy four-state machine collapsed to three; "Pending"
// replaces both "Dirty" and "Local". The state literal here is 0,
// matching blockstore.BlockStatePending.
func (s *PostgresMetadataStore) ListPending(ctx context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
	// olderThan <= 0 means "no age filter" — return every local block. The
	// age predicate is omitted entirely in that case to avoid the corner
	// where created_at ties or beats time.Now() under tight scheduling.
	query := `SELECT id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at
		FROM file_blocks
		WHERE state = 0 /* Pending */ AND cache_path IS NOT NULL`
	args := make([]any, 0, 2)
	if olderThan > 0 {
		args = append(args, time.Now().Add(-olderThan))
		query += fmt.Sprintf(" AND created_at < $%d", len(args))
	}
	query += " ORDER BY created_at ASC"
	if limit > 0 {
		args = append(args, limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list pending blocks: %w", err)
	}
	defer rows.Close()
	return scanFileBlockRows(rows)
}

// ListFileBlocks returns all blocks belonging to a file, ordered by block index.
// Uses LIKE query on block ID prefix, then sorts in Go for correct numeric ordering.
// Not on the narrowed FileBlockStore interface;
// kept as a backend method for engine-internal callers.
func (s *PostgresMetadataStore) ListFileBlocks(ctx context.Context, payloadID string) ([]*metadata.FileBlock, error) {
	query := `SELECT id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at
		FROM file_blocks
		WHERE id LIKE $1
		ORDER BY id ASC`
	rows, err := s.query(ctx, query, payloadID+"/%")
	if err != nil {
		return nil, fmt.Errorf("list file blocks: %w", err)
	}
	defer rows.Close()
	result, err := scanFileBlockRows(rows)
	if err != nil {
		return nil, err
	}
	// SQL ORDER BY id ASC gives lexicographic order which is wrong for multi-digit
	// block indices (e.g., "10" < "2"). Sort by parsed numeric index.
	sort.Slice(result, func(i, j int) bool {
		return pgParseBlockIdx(result[i].ID) < pgParseBlockIdx(result[j].ID)
	})
	if result == nil {
		return []*metadata.FileBlock{}, nil
	}
	return result, nil
}

// enumerateHashesQuery is the GC mark live-set query. It UNIONs the CAS index
// (file_blocks.hash, VARCHAR hex) with the per-file manifest
// (file_block_refs.hash, BYTEA → encode hex) so the live set is a strict
// SUPERSET of both structures. The snapshot Backup HashSet is built from
// file_block_refs alone (File.Blocks); without the union a hash present only in
// file_block_refs (e.g. a manifest row whose CAS index row was never written or
// already reaped) would be absent from the live set and the GC sweep would reap
// the still-live remote chunk once a snapshot hold lapsed (data loss). NULL
// hashes (legacy pre-CAS file_blocks rows) are emitted as the zero ContentHash
// and skipped by the mark phase; file_block_refs.hash is NOT NULL.
//
// UNION ALL, not UNION: the consumer (GC mark phase) dedupes hashes into a set,
// so cross-source and intra-source duplicates are harmless. UNION would force an
// expensive sort/hash-aggregate to dedupe at the query layer for no benefit.
const enumerateHashesQuery = `SELECT hash FROM file_blocks
UNION ALL
SELECT encode(hash, 'hex') FROM file_block_refs`

// EnumerateFileBlocks streams every live-set ContentHash through fn, unioning
// the CAS index with the per-file manifest (see enumerateHashesQuery).
func (s *PostgresMetadataStore) EnumerateFileBlocks(ctx context.Context, fn func(blockstore.ContentHash) error) error {
	rows, err := s.query(ctx, enumerateHashesQuery)
	if err != nil {
		return fmt.Errorf("enumerate file blocks: query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("enumerate file blocks: %w", err)
		}
		var hashStr sql.NullString
		if err := rows.Scan(&hashStr); err != nil {
			return fmt.Errorf("enumerate file blocks: scan: %w", err)
		}
		var h blockstore.ContentHash
		if hashStr.Valid {
			parsed, perr := metadata.ParseContentHash(hashStr.String)
			if perr != nil {
				// (mark fail-closed): a malformed hash row
				// cannot be silently coerced to the zero hash — that would
				// invite the GC mark phase to treat the row as a legacy
				// pre-CAS entry and the sweep would reap a still-live CAS
				// object once the grace TTL lapses. Surface the parse error
				// so EnumerateFileBlocks aborts and the sweep is skipped.
				return fmt.Errorf("enumerate file blocks: parse hash %q: %w",
					hashStr.String, perr)
			}
			h = parsed
		}
		if err := fn(h); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("enumerate file blocks: rows: %w", err)
	}
	return nil
}

// pgParseBlockIdx extracts the numeric block index from a block ID ("{payloadID}/{blockIdx}").
func pgParseBlockIdx(id string) int {
	if idx := strings.LastIndex(id, "/"); idx >= 0 {
		var v int
		if _, err := fmt.Sscanf(id[idx+1:], "%d", &v); err == nil {
			return v
		}
	}
	return 0
}

// ============================================================================
// Scan Helpers
// ============================================================================

// scanFileBlock scans a single row into a FileBlock.
func scanFileBlock(row pgx.Row) (*metadata.FileBlock, error) {
	var (
		block             metadata.FileBlock
		hashStr           sql.NullString
		cachePath         sql.NullString
		blockStoreKey     sql.NullString
		lastSyncAttemptAt sql.NullTime
	)
	if err := row.Scan(&block.ID, &hashStr, &block.DataSize, &cachePath, &blockStoreKey,
		&block.RefCount, &block.LastAccess, &block.CreatedAt, &block.State, &lastSyncAttemptAt); err != nil {
		return nil, err
	}
	if hashStr.Valid {
		// do not silently coerce malformed CAS hashes to the
		// zero hash — see EnumerateFileBlocks for the data-loss scenario.
		h, perr := metadata.ParseContentHash(hashStr.String)
		if perr != nil {
			return nil, fmt.Errorf("scan file block %s: parse hash %q: %w",
				block.ID, hashStr.String, perr)
		}
		block.Hash = h
	}
	if cachePath.Valid {
		block.LocalPath = cachePath.String
	}
	if blockStoreKey.Valid {
		block.BlockStoreKey = blockStoreKey.String
	}
	if lastSyncAttemptAt.Valid {
		block.LastSyncAttemptAt = lastSyncAttemptAt.Time
	}
	return &block, nil
}

// scanFileBlockRows scans multiple rows into FileBlock slices.
func scanFileBlockRows(rows pgx.Rows) ([]*metadata.FileBlock, error) {
	var result []*metadata.FileBlock
	for rows.Next() {
		block, err := scanFileBlock(rows)
		if err != nil {
			return nil, fmt.Errorf("scan file block: %w", err)
		}
		result = append(result, block)
	}
	return result, rows.Err()
}

// ============================================================================
// Transaction Support
// ============================================================================

// Ensure postgresTransaction implements FileBlockStore
var _ blockstore.FileBlockStore = (*postgresTransaction)(nil)

// (review iteration 1): the FileBlockStore methods on
// postgresTransaction MUST execute against the txn's own pgx.Tx, not the
// public store's connection-pool helpers. Previously every method just
// called `tx.store.X(...)` which routed through the pool — defeating
// rollback for any caller that bumped RefCount inside WithTransaction
// then encountered a downstream PutFile failure (silent
// leak). All proxies below are now tx-bound; non-mutating
// helpers that don't support tx-binding here (ListPending, etc.) keep
// the pool path because no caller mutates state through them.

func (tx *postgresTransaction) GetFileBlock(ctx context.Context, id string) (*metadata.FileBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	query := `SELECT id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at
		FROM file_blocks WHERE id = $1`
	row := tx.tx.QueryRow(ctx, query, id)
	block, err := scanFileBlock(row)
	if err == pgx.ErrNoRows {
		return nil, metadata.ErrFileBlockNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get file block: %w", err)
	}
	return block, nil
}

func (tx *postgresTransaction) Put(ctx context.Context, block *metadata.FileBlock) error {
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
	var cachePath *string
	if block.LocalPath != "" {
		cachePath = &block.LocalPath
	}
	var lastSyncAttemptAt *time.Time
	if !block.LastSyncAttemptAt.IsZero() {
		t := block.LastSyncAttemptAt
		lastSyncAttemptAt = &t
	}
	// omit ref_count from the ON CONFLICT update list (matches
	// the pool-path Put). RefCount mutates only via Increment/Decrement.
	query := `
		INSERT INTO file_blocks (id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (id) DO UPDATE SET
			hash = EXCLUDED.hash,
			data_size = EXCLUDED.data_size,
			cache_path = EXCLUDED.cache_path,
			block_store_key = EXCLUDED.block_store_key,
			last_access = EXCLUDED.last_access,
			state = EXCLUDED.state,
			last_sync_attempt_at = EXCLUDED.last_sync_attempt_at`
	_, err := tx.tx.Exec(ctx, query,
		block.ID, hashStr, block.DataSize, cachePath, blockStoreKey,
		block.RefCount, block.LastAccess, block.CreatedAt, block.State, lastSyncAttemptAt)
	if err != nil {
		return fmt.Errorf("put file block: %w", err)
	}
	return nil
}

func (tx *postgresTransaction) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := tx.tx.Exec(ctx, `DELETE FROM file_blocks WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete file block: %w", err)
	}
	if result.RowsAffected() == 0 {
		return metadata.ErrFileBlockNotFound
	}
	return nil
}

// IncrementRefCount runs the +1 UPDATE on the active pgx.Tx so a
// subsequent rollback undoes the bump (fix). Production callers
// route here through metadataCoordinator.IncrementRefCount when ctx
// carries an active tx via metadata.WithTx.
func (tx *postgresTransaction) IncrementRefCount(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := tx.tx.Exec(ctx,
		`UPDATE file_blocks SET ref_count = ref_count + 1 WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("increment ref count: %w", err)
	}
	if result.RowsAffected() == 0 {
		return metadata.ErrFileBlockNotFound
	}
	return nil
}

// DecrementRefCount runs the -1 UPDATE on the active pgx.Tx so a
// subsequent rollback undoes the decrement (fix).
func (tx *postgresTransaction) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	query := `UPDATE file_blocks SET ref_count = GREATEST(ref_count - 1, 0) WHERE id = $1 RETURNING ref_count`
	var newCount uint32
	err := tx.tx.QueryRow(ctx, query, id).Scan(&newCount)
	if err == pgx.ErrNoRows {
		return 0, metadata.ErrFileBlockNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("decrement ref count: %w", err)
	}
	return newCount, nil
}

// DecrementRefCountAndReap runs the -1 UPDATE + reap-at-zero DELETE on the
// active pgx.Tx so a subsequent rollback undoes both. Returns (0, nil) when the
// row is already absent.
func (tx *postgresTransaction) DecrementRefCountAndReap(ctx context.Context, id string) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return decrementAndReapTx(ctx, tx.tx, id)
}

// AddRef runs the +1 UPDATE keyed by hash on the active pgx.Tx so a
// subsequent rollback undoes the bump (parity for the
// LRU hit path). Returns metadata.ErrUnknownHash when no row matches.
func (tx *postgresTransaction) AddRef(ctx context.Context, hash blockstore.ContentHash, _ string, _ blockstore.BlockRef) error {
	// payloadID + blockRef accepted for future GC traceability;
	// postgres backend records ref count only — parameters intentionally
	// blanked.
	if err := ctx.Err(); err != nil {
		return err
	}
	// state = 2 (Remote) scoping mirrors the pool-path AddRef and the
	// memory/badger backends — a Pending row is not a valid dedup donor.
	result, err := tx.tx.Exec(ctx,
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

func (tx *postgresTransaction) GetByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	query := `SELECT id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at
		FROM file_blocks WHERE hash = $1 AND state = 2 /* Remote */`
	row := tx.tx.QueryRow(ctx, query, hash.String())
	block, err := scanFileBlock(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find file block by hash: %w", err)
	}
	return block, nil
}

// ListPending / ListFileBlocks / EnumerateFileBlocks run on the active
// transaction (tx.tx), NOT the pool. Delegating to the pool opens a separate
// connection that cannot see this transaction's uncommitted writes, so a Put
// followed by a List in the same WithTransaction would miss the pending row
// (read-after-write violation; the SQL is otherwise identical to the
// store-level methods).
func (tx *postgresTransaction) ListPending(ctx context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
	query := `SELECT id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at
		FROM file_blocks
		WHERE state = 0 /* Pending */ AND cache_path IS NOT NULL`
	args := make([]any, 0, 2)
	if olderThan > 0 {
		args = append(args, time.Now().Add(-olderThan))
		query += fmt.Sprintf(" AND created_at < $%d", len(args))
	}
	query += " ORDER BY created_at ASC"
	if limit > 0 {
		args = append(args, limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	rows, err := tx.tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list pending blocks: %w", err)
	}
	defer rows.Close()
	return scanFileBlockRows(rows)
}

func (tx *postgresTransaction) ListFileBlocks(ctx context.Context, payloadID string) ([]*metadata.FileBlock, error) {
	query := `SELECT id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at
		FROM file_blocks
		WHERE id LIKE $1
		ORDER BY id ASC`
	rows, err := tx.tx.Query(ctx, query, payloadID+"/%")
	if err != nil {
		return nil, fmt.Errorf("list file blocks: %w", err)
	}
	defer rows.Close()
	result, err := scanFileBlockRows(rows)
	if err != nil {
		return nil, err
	}
	// Lexicographic SQL order mis-sorts multi-digit indices ("10" < "2");
	// sort by parsed numeric index, matching the store-level method.
	sort.Slice(result, func(i, j int) bool {
		return pgParseBlockIdx(result[i].ID) < pgParseBlockIdx(result[j].ID)
	})
	if result == nil {
		return []*metadata.FileBlock{}, nil
	}
	return result, nil
}

func (tx *postgresTransaction) EnumerateFileBlocks(ctx context.Context, fn func(blockstore.ContentHash) error) error {
	rows, err := tx.tx.Query(ctx, enumerateHashesQuery)
	if err != nil {
		return fmt.Errorf("enumerate file blocks: query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("enumerate file blocks: %w", err)
		}
		var hashStr sql.NullString
		if err := rows.Scan(&hashStr); err != nil {
			return fmt.Errorf("enumerate file blocks: scan: %w", err)
		}
		var h blockstore.ContentHash
		if hashStr.Valid {
			parsed, perr := metadata.ParseContentHash(hashStr.String)
			if perr != nil {
				return fmt.Errorf("enumerate file blocks: parse hash %q: %w", hashStr.String, perr)
			}
			h = parsed
		}
		if err := fn(h); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("enumerate file blocks: rows: %w", err)
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
func (s *PostgresMetadataStore) InjectCorruptHashRow(ctx context.Context, blockID string, badHash string) error {
	now := time.Now()
	_, err := s.exec(ctx, `
		INSERT INTO file_blocks (id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (id) DO UPDATE SET hash = EXCLUDED.hash`,
		blockID, badHash, uint32(64), nil, nil, uint32(1), now, now, int(blockstore.BlockStateRemote), nil,
	)
	if err != nil {
		return fmt.Errorf("inject corrupt hash row: %w", err)
	}
	return nil
}
