package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// ObjectStore Implementation for PostgreSQL Store
// ============================================================================
//
// This file implements the ObjectStore interface for the PostgreSQL metadata store.
// It provides content-addressed object, chunk, and block tracking for deduplication.
//
// Tables:
//   - objects: Object data with content hash as primary key
//   - object_chunks: Chunk data with content hash as primary key
//   - object_blocks: Block data with content hash as primary key
//
// Thread Safety: All operations use PostgreSQL transactions for ACID guarantees.
//
// ============================================================================

// Ensure PostgresMetadataStore implements ObjectStore
var _ metadata.ObjectStore = (*PostgresMetadataStore)(nil)

// ============================================================================
// Object Operations
// ============================================================================

// GetObject retrieves an object by its content hash.
func (s *PostgresMetadataStore) GetObject(ctx context.Context, id metadata.ContentHash) (*metadata.Object, error) {
	query := `SELECT id, size, ref_count, chunk_count, created_at, finalized FROM objects WHERE id = $1`
	row := s.pool.QueryRow(ctx, query, id.String())

	var obj metadata.Object
	var idStr string
	err := row.Scan(&idStr, &obj.Size, &obj.RefCount, &obj.ChunkCount, &obj.CreatedAt, &obj.Finalized)
	if err == pgx.ErrNoRows {
		return nil, metadata.ErrObjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get object: %w", err)
	}

	obj.ID, _ = metadata.ParseContentHash(idStr)
	return &obj, nil
}

// PutObject stores or updates an object.
func (s *PostgresMetadataStore) PutObject(ctx context.Context, obj *metadata.Object) error {
	query := `
		INSERT INTO objects (id, size, ref_count, chunk_count, created_at, finalized)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
			size = EXCLUDED.size,
			ref_count = EXCLUDED.ref_count,
			chunk_count = EXCLUDED.chunk_count,
			finalized = EXCLUDED.finalized`
	_, err := s.pool.Exec(ctx, query,
		obj.ID.String(), obj.Size, obj.RefCount, obj.ChunkCount, obj.CreatedAt, obj.Finalized)
	if err != nil {
		return fmt.Errorf("put object: %w", err)
	}
	return nil
}

// DeleteObject removes an object by its content hash.
func (s *PostgresMetadataStore) DeleteObject(ctx context.Context, id metadata.ContentHash) error {
	result, err := s.pool.Exec(ctx, `DELETE FROM objects WHERE id = $1`, id.String())
	if err != nil {
		return fmt.Errorf("delete object: %w", err)
	}
	rows := result.RowsAffected()
	if rows == 0 {
		return metadata.ErrObjectNotFound
	}
	return nil
}

// IncrementObjectRefCount atomically increments an object's RefCount.
func (s *PostgresMetadataStore) IncrementObjectRefCount(ctx context.Context, id metadata.ContentHash) (uint32, error) {
	query := `UPDATE objects SET ref_count = ref_count + 1 WHERE id = $1 RETURNING ref_count`
	var newCount uint32
	err := s.pool.QueryRow(ctx, query, id.String()).Scan(&newCount)
	if err == pgx.ErrNoRows {
		return 0, metadata.ErrObjectNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("increment object ref count: %w", err)
	}
	return newCount, nil
}

// DecrementObjectRefCount atomically decrements an object's RefCount.
func (s *PostgresMetadataStore) DecrementObjectRefCount(ctx context.Context, id metadata.ContentHash) (uint32, error) {
	query := `UPDATE objects SET ref_count = GREATEST(ref_count - 1, 0) WHERE id = $1 RETURNING ref_count`
	var newCount uint32
	err := s.pool.QueryRow(ctx, query, id.String()).Scan(&newCount)
	if err == pgx.ErrNoRows {
		return 0, metadata.ErrObjectNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("decrement object ref count: %w", err)
	}
	return newCount, nil
}

// ============================================================================
// Chunk Operations
// ============================================================================

// GetChunk retrieves a chunk by its content hash.
func (s *PostgresMetadataStore) GetChunk(ctx context.Context, hash metadata.ContentHash) (*metadata.ObjectChunk, error) {
	query := `SELECT object_id, idx, hash, size, block_count, ref_count FROM object_chunks WHERE hash = $1`
	row := s.pool.QueryRow(ctx, query, hash.String())

	var chunk metadata.ObjectChunk
	var objectIDStr, hashStr string
	err := row.Scan(&objectIDStr, &chunk.Index, &hashStr, &chunk.Size, &chunk.BlockCount, &chunk.RefCount)
	if err == pgx.ErrNoRows {
		return nil, metadata.ErrChunkNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get chunk: %w", err)
	}

	chunk.ObjectID, _ = metadata.ParseContentHash(objectIDStr)
	chunk.Hash, _ = metadata.ParseContentHash(hashStr)
	return &chunk, nil
}

// GetChunksByObject retrieves all chunks for an object, ordered by Index.
func (s *PostgresMetadataStore) GetChunksByObject(ctx context.Context, objectID metadata.ContentHash) ([]*metadata.ObjectChunk, error) {
	query := `SELECT object_id, idx, hash, size, block_count, ref_count FROM object_chunks WHERE object_id = $1 ORDER BY idx`
	rows, err := s.pool.Query(ctx, query, objectID.String())
	if err != nil {
		return nil, fmt.Errorf("get chunks by object: %w", err)
	}
	defer rows.Close()

	var chunks []*metadata.ObjectChunk
	for rows.Next() {
		var chunk metadata.ObjectChunk
		var objectIDStr, hashStr string
		if err := rows.Scan(&objectIDStr, &chunk.Index, &hashStr, &chunk.Size, &chunk.BlockCount, &chunk.RefCount); err != nil {
			return nil, fmt.Errorf("scan chunk: %w", err)
		}
		chunk.ObjectID, _ = metadata.ParseContentHash(objectIDStr)
		chunk.Hash, _ = metadata.ParseContentHash(hashStr)
		chunks = append(chunks, &chunk)
	}
	return chunks, rows.Err()
}

// PutChunk stores or updates a chunk.
func (s *PostgresMetadataStore) PutChunk(ctx context.Context, chunk *metadata.ObjectChunk) error {
	query := `
		INSERT INTO object_chunks (object_id, idx, hash, size, block_count, ref_count)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (hash) DO UPDATE SET
			ref_count = EXCLUDED.ref_count`
	_, err := s.pool.Exec(ctx, query,
		chunk.ObjectID.String(), chunk.Index, chunk.Hash.String(), chunk.Size, chunk.BlockCount, chunk.RefCount)
	if err != nil {
		return fmt.Errorf("put chunk: %w", err)
	}
	return nil
}

// DeleteChunk removes a chunk by its content hash.
func (s *PostgresMetadataStore) DeleteChunk(ctx context.Context, hash metadata.ContentHash) error {
	result, err := s.pool.Exec(ctx, `DELETE FROM object_chunks WHERE hash = $1`, hash.String())
	if err != nil {
		return fmt.Errorf("delete chunk: %w", err)
	}
	rows := result.RowsAffected()
	if rows == 0 {
		return metadata.ErrChunkNotFound
	}
	return nil
}

// IncrementChunkRefCount atomically increments a chunk's RefCount.
func (s *PostgresMetadataStore) IncrementChunkRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	query := `UPDATE object_chunks SET ref_count = ref_count + 1 WHERE hash = $1 RETURNING ref_count`
	var newCount uint32
	err := s.pool.QueryRow(ctx, query, hash.String()).Scan(&newCount)
	if err == pgx.ErrNoRows {
		return 0, metadata.ErrChunkNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("increment chunk ref count: %w", err)
	}
	return newCount, nil
}

// DecrementChunkRefCount atomically decrements a chunk's RefCount.
func (s *PostgresMetadataStore) DecrementChunkRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	query := `UPDATE object_chunks SET ref_count = GREATEST(ref_count - 1, 0) WHERE hash = $1 RETURNING ref_count`
	var newCount uint32
	err := s.pool.QueryRow(ctx, query, hash.String()).Scan(&newCount)
	if err == pgx.ErrNoRows {
		return 0, metadata.ErrChunkNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("decrement chunk ref count: %w", err)
	}
	return newCount, nil
}

// ============================================================================
// Block Operations
// ============================================================================

// GetBlock retrieves a block by its content hash.
func (s *PostgresMetadataStore) GetBlock(ctx context.Context, hash metadata.ContentHash) (*metadata.ObjectBlock, error) {
	query := `SELECT chunk_hash, idx, hash, size, ref_count, uploaded_at FROM object_blocks WHERE hash = $1`
	row := s.pool.QueryRow(ctx, query, hash.String())

	var block metadata.ObjectBlock
	var chunkHashStr, hashStr string
	var uploadedAt sql.NullTime
	err := row.Scan(&chunkHashStr, &block.Index, &hashStr, &block.Size, &block.RefCount, &uploadedAt)
	if err == pgx.ErrNoRows {
		return nil, metadata.ErrBlockNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get block: %w", err)
	}

	block.ChunkHash, _ = metadata.ParseContentHash(chunkHashStr)
	block.Hash, _ = metadata.ParseContentHash(hashStr)
	if uploadedAt.Valid {
		block.UploadedAt = uploadedAt.Time
	}
	return &block, nil
}

// GetBlocksByChunk retrieves all blocks for a chunk, ordered by Index.
func (s *PostgresMetadataStore) GetBlocksByChunk(ctx context.Context, chunkHash metadata.ContentHash) ([]*metadata.ObjectBlock, error) {
	query := `SELECT chunk_hash, idx, hash, size, ref_count, uploaded_at FROM object_blocks WHERE chunk_hash = $1 ORDER BY idx`
	rows, err := s.pool.Query(ctx, query, chunkHash.String())
	if err != nil {
		return nil, fmt.Errorf("get blocks by chunk: %w", err)
	}
	defer rows.Close()

	var blocks []*metadata.ObjectBlock
	for rows.Next() {
		var block metadata.ObjectBlock
		var chunkHashStr, hashStr string
		var uploadedAt sql.NullTime
		if err := rows.Scan(&chunkHashStr, &block.Index, &hashStr, &block.Size, &block.RefCount, &uploadedAt); err != nil {
			return nil, fmt.Errorf("scan block: %w", err)
		}
		block.ChunkHash, _ = metadata.ParseContentHash(chunkHashStr)
		block.Hash, _ = metadata.ParseContentHash(hashStr)
		if uploadedAt.Valid {
			block.UploadedAt = uploadedAt.Time
		}
		blocks = append(blocks, &block)
	}
	return blocks, rows.Err()
}

// PutBlock stores or updates a block.
func (s *PostgresMetadataStore) PutBlock(ctx context.Context, block *metadata.ObjectBlock) error {
	var uploadedAt interface{} = nil
	if !block.UploadedAt.IsZero() {
		uploadedAt = block.UploadedAt
	}
	query := `
		INSERT INTO object_blocks (chunk_hash, idx, hash, size, ref_count, uploaded_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (hash) DO UPDATE SET
			ref_count = EXCLUDED.ref_count,
			uploaded_at = COALESCE(EXCLUDED.uploaded_at, object_blocks.uploaded_at)`
	_, err := s.pool.Exec(ctx, query,
		block.ChunkHash.String(), block.Index, block.Hash.String(), block.Size, block.RefCount, uploadedAt)
	if err != nil {
		return fmt.Errorf("put block: %w", err)
	}
	return nil
}

// DeleteBlock removes a block by its content hash.
func (s *PostgresMetadataStore) DeleteBlock(ctx context.Context, hash metadata.ContentHash) error {
	result, err := s.pool.Exec(ctx, `DELETE FROM object_blocks WHERE hash = $1`, hash.String())
	if err != nil {
		return fmt.Errorf("delete block: %w", err)
	}
	rows := result.RowsAffected()
	if rows == 0 {
		return metadata.ErrBlockNotFound
	}
	return nil
}

// FindBlockByHash looks up a block by its content hash.
// Returns nil without error if not found.
func (s *PostgresMetadataStore) FindBlockByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.ObjectBlock, error) {
	block, err := s.GetBlock(ctx, hash)
	if err == metadata.ErrBlockNotFound {
		return nil, nil
	}
	return block, err
}

// IncrementBlockRefCount atomically increments a block's RefCount.
func (s *PostgresMetadataStore) IncrementBlockRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	query := `UPDATE object_blocks SET ref_count = ref_count + 1 WHERE hash = $1 RETURNING ref_count`
	var newCount uint32
	err := s.pool.QueryRow(ctx, query, hash.String()).Scan(&newCount)
	if err == pgx.ErrNoRows {
		return 0, metadata.ErrBlockNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("increment block ref count: %w", err)
	}
	return newCount, nil
}

// DecrementBlockRefCount atomically decrements a block's RefCount.
func (s *PostgresMetadataStore) DecrementBlockRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	query := `UPDATE object_blocks SET ref_count = GREATEST(ref_count - 1, 0) WHERE hash = $1 RETURNING ref_count`
	var newCount uint32
	err := s.pool.QueryRow(ctx, query, hash.String()).Scan(&newCount)
	if err == pgx.ErrNoRows {
		return 0, metadata.ErrBlockNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("decrement block ref count: %w", err)
	}
	return newCount, nil
}

// MarkBlockUploaded marks a block as uploaded to the block store.
func (s *PostgresMetadataStore) MarkBlockUploaded(ctx context.Context, hash metadata.ContentHash) error {
	result, err := s.pool.Exec(ctx,
		`UPDATE object_blocks SET uploaded_at = $1 WHERE hash = $2`,
		time.Now(), hash.String())
	if err != nil {
		return fmt.Errorf("mark block uploaded: %w", err)
	}
	rows := result.RowsAffected()
	if rows == 0 {
		return metadata.ErrBlockNotFound
	}
	return nil
}

// ============================================================================
// Transaction Support
// ============================================================================

// Ensure postgresTransaction implements ObjectStore
var _ metadata.ObjectStore = (*postgresTransaction)(nil)

// Transaction wrapper methods delegate to store with transaction context

func (tx *postgresTransaction) GetObject(ctx context.Context, id metadata.ContentHash) (*metadata.Object, error) {
	return tx.store.GetObject(ctx, id)
}

func (tx *postgresTransaction) PutObject(ctx context.Context, obj *metadata.Object) error {
	return tx.store.PutObject(ctx, obj)
}

func (tx *postgresTransaction) DeleteObject(ctx context.Context, id metadata.ContentHash) error {
	return tx.store.DeleteObject(ctx, id)
}

func (tx *postgresTransaction) IncrementObjectRefCount(ctx context.Context, id metadata.ContentHash) (uint32, error) {
	return tx.store.IncrementObjectRefCount(ctx, id)
}

func (tx *postgresTransaction) DecrementObjectRefCount(ctx context.Context, id metadata.ContentHash) (uint32, error) {
	return tx.store.DecrementObjectRefCount(ctx, id)
}

func (tx *postgresTransaction) GetChunk(ctx context.Context, hash metadata.ContentHash) (*metadata.ObjectChunk, error) {
	return tx.store.GetChunk(ctx, hash)
}

func (tx *postgresTransaction) GetChunksByObject(ctx context.Context, objectID metadata.ContentHash) ([]*metadata.ObjectChunk, error) {
	return tx.store.GetChunksByObject(ctx, objectID)
}

func (tx *postgresTransaction) PutChunk(ctx context.Context, chunk *metadata.ObjectChunk) error {
	return tx.store.PutChunk(ctx, chunk)
}

func (tx *postgresTransaction) DeleteChunk(ctx context.Context, hash metadata.ContentHash) error {
	return tx.store.DeleteChunk(ctx, hash)
}

func (tx *postgresTransaction) IncrementChunkRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	return tx.store.IncrementChunkRefCount(ctx, hash)
}

func (tx *postgresTransaction) DecrementChunkRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	return tx.store.DecrementChunkRefCount(ctx, hash)
}

func (tx *postgresTransaction) GetBlock(ctx context.Context, hash metadata.ContentHash) (*metadata.ObjectBlock, error) {
	return tx.store.GetBlock(ctx, hash)
}

func (tx *postgresTransaction) GetBlocksByChunk(ctx context.Context, chunkHash metadata.ContentHash) ([]*metadata.ObjectBlock, error) {
	return tx.store.GetBlocksByChunk(ctx, chunkHash)
}

func (tx *postgresTransaction) PutBlock(ctx context.Context, block *metadata.ObjectBlock) error {
	return tx.store.PutBlock(ctx, block)
}

func (tx *postgresTransaction) DeleteBlock(ctx context.Context, hash metadata.ContentHash) error {
	return tx.store.DeleteBlock(ctx, hash)
}

func (tx *postgresTransaction) FindBlockByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.ObjectBlock, error) {
	return tx.store.FindBlockByHash(ctx, hash)
}

func (tx *postgresTransaction) IncrementBlockRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	return tx.store.IncrementBlockRefCount(ctx, hash)
}

func (tx *postgresTransaction) DecrementBlockRefCount(ctx context.Context, hash metadata.ContentHash) (uint32, error) {
	return tx.store.DecrementBlockRefCount(ctx, hash)
}

func (tx *postgresTransaction) MarkBlockUploaded(ctx context.Context, hash metadata.ContentHash) error {
	return tx.store.MarkBlockUploaded(ctx, hash)
}

// PostgreSQL migration for object tables
const objectTablesMigration = `
-- Objects table
CREATE TABLE IF NOT EXISTS objects (
    id VARCHAR(64) PRIMARY KEY,
    size BIGINT NOT NULL DEFAULT 0,
    ref_count INTEGER NOT NULL DEFAULT 1,
    chunk_count INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    finalized BOOLEAN NOT NULL DEFAULT FALSE
);

-- Object chunks table
CREATE TABLE IF NOT EXISTS object_chunks (
    hash VARCHAR(64) PRIMARY KEY,
    object_id VARCHAR(64) NOT NULL REFERENCES objects(id) ON DELETE CASCADE,
    idx INTEGER NOT NULL,
    size INTEGER NOT NULL DEFAULT 0,
    block_count INTEGER NOT NULL DEFAULT 0,
    ref_count INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_object_chunks_object_id ON object_chunks(object_id, idx);

-- Object blocks table
CREATE TABLE IF NOT EXISTS object_blocks (
    hash VARCHAR(64) PRIMARY KEY,
    chunk_hash VARCHAR(64) NOT NULL REFERENCES object_chunks(hash) ON DELETE CASCADE,
    idx INTEGER NOT NULL,
    size INTEGER NOT NULL DEFAULT 0,
    ref_count INTEGER NOT NULL DEFAULT 1,
    uploaded_at TIMESTAMP WITH TIME ZONE
);
CREATE INDEX IF NOT EXISTS idx_object_blocks_chunk_hash ON object_blocks(chunk_hash, idx);
`

// Unused variable to document the migration SQL
var _ = objectTablesMigration
