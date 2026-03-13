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
	query := `SELECT id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state
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
	if block.CachePath != "" {
		cachePath = &block.CachePath
	}

	query := `
		INSERT INTO file_blocks (id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (id) DO UPDATE SET
			hash = EXCLUDED.hash,
			data_size = EXCLUDED.data_size,
			cache_path = EXCLUDED.cache_path,
			block_store_key = EXCLUDED.block_store_key,
			ref_count = EXCLUDED.ref_count,
			last_access = EXCLUDED.last_access,
			state = EXCLUDED.state`
	_, err := s.exec(ctx, query,
		block.ID, hashStr, block.DataSize, cachePath, blockStoreKey,
		block.RefCount, block.LastAccess, block.CreatedAt, block.State)
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
func (s *PostgresMetadataStore) FindFileBlockByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileBlock, error) {
	query := `SELECT id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state
		FROM file_blocks WHERE hash = $1 AND state = 3 /* Remote */`
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

// ListLocalBlocks returns blocks in Local state (complete, on disk, not yet
// synced to remote) older than the given duration.
// If limit > 0, at most limit blocks are returned.
func (s *PostgresMetadataStore) ListLocalBlocks(ctx context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
	cutoff := time.Now().Add(-olderThan)
	var query string
	var rows pgx.Rows
	var err error
	if limit > 0 {
		query = `SELECT id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state
			FROM file_blocks
			WHERE state = 1 /* Local */ AND cache_path IS NOT NULL AND created_at < $1
			ORDER BY created_at ASC
			LIMIT $2`
		rows, err = s.query(ctx, query, cutoff, limit)
	} else {
		query = `SELECT id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state
			FROM file_blocks
			WHERE state = 1 /* Local */ AND cache_path IS NOT NULL AND created_at < $1
			ORDER BY created_at ASC`
		rows, err = s.query(ctx, query, cutoff)
	}
	if err != nil {
		return nil, fmt.Errorf("list local blocks: %w", err)
	}
	defer rows.Close()
	return scanFileBlockRows(rows)
}

// ListRemoteBlocks returns blocks that are both cached locally and confirmed
// in remote store, ordered by LRU (oldest LastAccess first).
// If limit > 0, returns at most that many rows; if limit <= 0, returns all.
func (s *PostgresMetadataStore) ListRemoteBlocks(ctx context.Context, limit int) ([]*metadata.FileBlock, error) {
	baseQuery := `SELECT id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state
		FROM file_blocks
		WHERE state = 3 /* Remote */ AND cache_path IS NOT NULL
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
	baseQuery := `SELECT id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state
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
	query := `SELECT id, hash, data_size, cache_path, block_store_key, ref_count, last_access, created_at, state
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
	var block metadata.FileBlock
	var hashStr sql.NullString
	var cachePath sql.NullString
	var blockStoreKey sql.NullString

	err := row.Scan(&block.ID, &hashStr, &block.DataSize, &cachePath, &blockStoreKey,
		&block.RefCount, &block.LastAccess, &block.CreatedAt, &block.State)
	if err != nil {
		return nil, err
	}

	if hashStr.Valid {
		block.Hash, _ = metadata.ParseContentHash(hashStr.String)
	}
	if cachePath.Valid {
		block.CachePath = cachePath.String
	}
	if blockStoreKey.Valid {
		block.BlockStoreKey = blockStoreKey.String
	}
	return &block, nil
}

// scanFileBlockRows scans multiple rows into FileBlock slices.
func scanFileBlockRows(rows pgx.Rows) ([]*metadata.FileBlock, error) {
	var result []*metadata.FileBlock
	for rows.Next() {
		var block metadata.FileBlock
		var hashStr sql.NullString
		var cachePath sql.NullString
		var blockStoreKey sql.NullString

		if err := rows.Scan(&block.ID, &hashStr, &block.DataSize, &cachePath, &blockStoreKey,
			&block.RefCount, &block.LastAccess, &block.CreatedAt, &block.State); err != nil {
			return nil, fmt.Errorf("scan file block: %w", err)
		}

		if hashStr.Valid {
			block.Hash, _ = metadata.ParseContentHash(hashStr.String)
		}
		if cachePath.Valid {
			block.CachePath = cachePath.String
		}
		if blockStoreKey.Valid {
			block.BlockStoreKey = blockStoreKey.String
		}
		result = append(result, &block)
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

// PostgreSQL migration for file_blocks table
// State values: 0=Dirty, 1=Local (was Sealed), 2=Syncing (was Uploading), 3=Remote (was Uploaded)
const fileBlocksTableMigration = `
-- File blocks table (replaces objects, object_chunks, object_blocks)
CREATE TABLE IF NOT EXISTS file_blocks (
    id VARCHAR(36) PRIMARY KEY,
    hash VARCHAR(64),
    data_size INTEGER NOT NULL DEFAULT 0,
    cache_path TEXT,
    block_store_key TEXT,
    ref_count INTEGER NOT NULL DEFAULT 1,
    last_access TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    state SMALLINT NOT NULL DEFAULT 0  -- 0=Dirty, 1=Local, 2=Syncing, 3=Remote
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_file_blocks_hash ON file_blocks(hash) WHERE hash IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_file_blocks_local ON file_blocks(created_at) WHERE state = 1 AND cache_path IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_file_blocks_remote ON file_blocks(last_access) WHERE state = 3 AND cache_path IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_file_blocks_unreferenced ON file_blocks(id) WHERE ref_count = 0;
`

// Unused variable to document the migration SQL
var _ = fileBlocksTableMigration
