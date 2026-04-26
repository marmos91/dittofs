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

// GetFileBlock retrieves a file block by its ID.
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

// PutFileBlock stores or updates a file block.
func (s *PostgresMetadataStore) PutFileBlock(ctx context.Context, block *metadata.FileBlock) error {
	var hashStr *string
	if block.IsFinalized() {
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
	// Phase 11 D-13/D-14: persist LastSyncAttemptAt as NULL when zero so the
	// janitor's WHERE last_sync_attempt_at < cutoff predicate excludes
	// never-claimed rows naturally instead of matching every Pending row.
	var lastSyncAttemptAt *time.Time
	if !block.LastSyncAttemptAt.IsZero() {
		t := block.LastSyncAttemptAt
		lastSyncAttemptAt = &t
	}

	query := `
		INSERT INTO file_blocks (id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (id) DO UPDATE SET
			hash = EXCLUDED.hash,
			data_size = EXCLUDED.data_size,
			cache_path = EXCLUDED.cache_path,
			block_store_key = EXCLUDED.block_store_key,
			ref_count = EXCLUDED.ref_count,
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

// DeleteFileBlock removes a file block by its ID.
func (s *PostgresMetadataStore) DeleteFileBlock(ctx context.Context, id string) error {
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

// FindFileBlockByHash looks up a finalized block by its content hash.
// Returns nil without error if not found.
//
// Phase 11 STATE-01: dedup matches only Remote (state=2) blocks — Pending
// or Syncing rows have not been confirmed on the remote and are unsafe
// dedup targets.
func (s *PostgresMetadataStore) FindFileBlockByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileBlock, error) {
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

// ListLocalBlocks returns blocks in Pending state (RefCount>=1, not yet
// uploaded — D-13) older than the given duration. If limit > 0, at most
// limit blocks are returned.
//
// Phase 11 STATE-01: the legacy four-state machine collapsed to three;
// "Pending" replaces both "Dirty" and "Local". The state literal here is 0,
// matching blockstore.BlockStatePending.
func (s *PostgresMetadataStore) ListLocalBlocks(ctx context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
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
		return nil, fmt.Errorf("list local blocks: %w", err)
	}
	defer rows.Close()
	return scanFileBlockRows(rows)
}

// ListRemoteBlocks returns blocks that are both cached locally and confirmed
// in remote store, ordered by LRU (oldest LastAccess first).
// If limit > 0, returns at most that many rows; if limit <= 0, returns all.
//
// Phase 11 STATE-01: state=2 is Remote (was 3 pre-collapse).
func (s *PostgresMetadataStore) ListRemoteBlocks(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	baseQuery := `SELECT id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at
		FROM file_blocks
		WHERE state = 2 /* Remote */ AND cache_path IS NOT NULL
		ORDER BY last_access ASC`

	var rows pgx.Rows
	var err error
	if limit > 0 {
		rows, err = s.query(ctx, baseQuery+` LIMIT $1`, limit)
	} else {
		rows, err = s.query(ctx, baseQuery)
	}
	if err != nil {
		return nil, fmt.Errorf("list remote blocks: %w", err)
	}
	defer rows.Close()
	return scanFileBlockRows(rows)
}

// ListUnreferenced returns blocks with RefCount=0.
// If limit > 0, returns at most that many rows; if limit <= 0, returns all.
func (s *PostgresMetadataStore) ListUnreferenced(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	baseQuery := `SELECT id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state, last_sync_attempt_at
		FROM file_blocks
		WHERE ref_count = 0`

	var rows pgx.Rows
	var err error
	if limit > 0 {
		rows, err = s.query(ctx, baseQuery+` LIMIT $1`, limit)
	} else {
		rows, err = s.query(ctx, baseQuery)
	}
	if err != nil {
		return nil, fmt.Errorf("list unreferenced: %w", err)
	}
	defer rows.Close()
	return scanFileBlockRows(rows)
}

// ListFileBlocks returns all blocks belonging to a file, ordered by block index.
// Uses LIKE query on block ID prefix, then sorts in Go for correct numeric ordering.
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

// EnumerateFileBlocks streams every FileBlock's ContentHash through fn using
// a row cursor over the file_blocks table. NULL hashes (legacy rows pre-CAS)
// are emitted as the zero ContentHash so the GC mark phase can skip them
// explicitly. See GC-01 / D-02.
func (s *PostgresMetadataStore) EnumerateFileBlocks(ctx context.Context, fn func(blockstore.ContentHash) error) error {
	rows, err := s.query(ctx, `SELECT hash FROM file_blocks ORDER BY id`)
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
				// Phase 11 INV-04 (mark fail-closed): a malformed hash row
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
		// Phase 11 INV-04: do not silently coerce malformed CAS hashes to the
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

func (tx *postgresTransaction) GetFileBlock(ctx context.Context, id string) (*metadata.FileBlock, error) {
	return tx.store.GetFileBlock(ctx, id)
}

func (tx *postgresTransaction) PutFileBlock(ctx context.Context, block *metadata.FileBlock) error {
	return tx.store.PutFileBlock(ctx, block)
}

func (tx *postgresTransaction) DeleteFileBlock(ctx context.Context, id string) error {
	return tx.store.DeleteFileBlock(ctx, id)
}

func (tx *postgresTransaction) IncrementRefCount(ctx context.Context, id string) error {
	return tx.store.IncrementRefCount(ctx, id)
}

func (tx *postgresTransaction) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	return tx.store.DecrementRefCount(ctx, id)
}

func (tx *postgresTransaction) FindFileBlockByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileBlock, error) {
	return tx.store.FindFileBlockByHash(ctx, hash)
}

func (tx *postgresTransaction) ListLocalBlocks(ctx context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
	return tx.store.ListLocalBlocks(ctx, olderThan, limit)
}

func (tx *postgresTransaction) ListRemoteBlocks(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	return tx.store.ListRemoteBlocks(ctx, limit)
}

func (tx *postgresTransaction) ListUnreferenced(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	return tx.store.ListUnreferenced(ctx, limit)
}

func (tx *postgresTransaction) ListFileBlocks(ctx context.Context, payloadID string) ([]*metadata.FileBlock, error) {
	return tx.store.ListFileBlocks(ctx, payloadID)
}

func (tx *postgresTransaction) EnumerateFileBlocks(ctx context.Context, fn func(blockstore.ContentHash) error) error {
	return tx.store.EnumerateFileBlocks(ctx, fn)
}

// The file_blocks table schema lives in
// pkg/metadata/store/postgres/migrations/000010_file_blocks.up.sql.

// InjectCorruptHashRow stores a file_blocks row whose hash column holds a
// syntactically malformed value. Test-only: implements the storetest
// CorruptHashInjector capability so the conformance suite can exercise
// INV-04 fail-closed enumeration. The TEXT column lets us bypass the
// PutFileBlock contract that always serializes a valid ContentHash.String().
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
