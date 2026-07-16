// BlockRecordStore implementation for the SQLite metadata store.  The interface
// is required by metadata.Transaction, so every method exists in two variants: a
// store-level (pool path) variant on *SQLiteMetadataStore and a
// transaction-scoped variant on *sqliteTransaction.
//
// Semantics match the memory backend exactly:
//   - PutBlockRecord — idempotent upsert.
//   - GetBlockRecord — (_, false, nil) on miss.
//   - DeleteBlockRecord — idempotent (missing row → nil).
//   - DecrLiveChunkCount — floors at 0; error when blockID absent.
//   - WalkBlockRecords — enumerates all rows in implementation-defined order.
//   - CommitBlock — delegates to metadata.DefaultCommitBlock.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Compile-time assertions: the store and its transaction both satisfy the
// interface.  If a method signature drifts these lines will fail to compile
// before any test runs.
var _ metadata.BlockRecordStore = (*SQLiteMetadataStore)(nil)
var _ metadata.BlockRecordStore = (*sqliteTransaction)(nil)

// ============================================================================
// Store-level BlockRecordStore (pool path)
// ============================================================================

// PutBlockRecord writes or overwrites the block record for rec.BlockID.
func (s *SQLiteMetadataStore) PutBlockRecord(ctx context.Context, rec block.BlockRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := s.exec(ctx,
		`INSERT INTO block_records (block_id, block_hash, length, live_chunk_count, sync_state)
		 VALUES (?1, ?2, ?3, ?4, ?5)
		 ON CONFLICT (block_id) DO UPDATE SET
		     block_hash       = EXCLUDED.block_hash,
		     length           = EXCLUDED.length,
		     live_chunk_count = EXCLUDED.live_chunk_count,
		     sync_state       = EXCLUDED.sync_state`,
		rec.BlockID, rec.BlockHash[:], rec.Length, rec.LiveChunkCount, int(rec.SyncState))
	if err != nil {
		return fmt.Errorf("sqlite block_records put: %w", err)
	}
	return nil
}

// GetBlockRecord retrieves the block record for blockID.
// Returns (_, false, nil) when no record exists.
func (s *SQLiteMetadataStore) GetBlockRecord(ctx context.Context, blockID string) (block.BlockRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.BlockRecord{}, false, err
	}
	rec, found, err := scanBlockRecord(s.queryRow(ctx,
		`SELECT block_id, block_hash, length, live_chunk_count, sync_state
		 FROM block_records WHERE block_id = ?1`,
		blockID))
	if err != nil {
		return block.BlockRecord{}, false, fmt.Errorf("sqlite block_records get: %w", err)
	}
	return rec, found, nil
}

// DeleteBlockRecord removes the block record for blockID. Idempotent.
func (s *SQLiteMetadataStore) DeleteBlockRecord(ctx context.Context, blockID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.exec(ctx,
		`DELETE FROM block_records WHERE block_id = ?1`,
		blockID); err != nil {
		return fmt.Errorf("sqlite block_records delete: %w", err)
	}
	return nil
}

// WalkBlockRecords calls fn for every stored block record.
func (s *SQLiteMetadataStore) WalkBlockRecords(ctx context.Context, fn func(block.BlockRecord) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rows, err := s.query(ctx,
		`SELECT block_id, block_hash, length, live_chunk_count, sync_state FROM block_records`)
	if err != nil {
		return fmt.Errorf("sqlite block_records walk: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		rec, _, err := scanBlockRecordRow(rows)
		if err != nil {
			return fmt.Errorf("sqlite block_records walk scan: %w", err)
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	return rows.Err()
}

// DecrLiveChunkCount atomically decrements LiveChunkCount, flooring at 0.
// Returns the remaining count.  Errors if blockID does not exist.
// Uses WithTransaction so the check + update is atomic under SQLite's busy-retry loop.
func (s *SQLiteMetadataStore) DecrLiveChunkCount(ctx context.Context, blockID string, delta uint32) (uint32, error) {
	var remaining uint32
	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		rem, err := tx.DecrLiveChunkCount(ctx, blockID, delta)
		if err != nil {
			return err
		}
		remaining = rem
		return nil
	})
	return remaining, err
}

// ============================================================================
// CommitBlock (store-level, delegates to DefaultCommitBlock)
// ============================================================================

// CommitBlock atomically writes rec within a single transaction, then marks
// each chunk synced.  Idempotent on BlockID.
func (s *SQLiteMetadataStore) CommitBlock(ctx context.Context, rec block.BlockRecord, chunks []block.BlockChunkCommit) error {
	return metadata.DefaultCommitBlock(ctx, s, rec, chunks, nil)
}

// ============================================================================
// Transaction-level BlockRecordStore
// ============================================================================

// PutBlockRecord writes or overwrites the block record for rec.BlockID.
func (tx *sqliteTransaction) PutBlockRecord(ctx context.Context, rec block.BlockRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := tx.tx.Exec(ctx,
		`INSERT INTO block_records (block_id, block_hash, length, live_chunk_count, sync_state)
		 VALUES (?1, ?2, ?3, ?4, ?5)
		 ON CONFLICT (block_id) DO UPDATE SET
		     block_hash       = EXCLUDED.block_hash,
		     length           = EXCLUDED.length,
		     live_chunk_count = EXCLUDED.live_chunk_count,
		     sync_state       = EXCLUDED.sync_state`,
		rec.BlockID, rec.BlockHash[:], rec.Length, rec.LiveChunkCount, int(rec.SyncState))
	if err != nil {
		return fmt.Errorf("sqlite tx block_records put: %w", err)
	}
	return nil
}

// GetBlockRecord retrieves the block record for blockID.
// Returns (_, false, nil) when no record exists.
func (tx *sqliteTransaction) GetBlockRecord(ctx context.Context, blockID string) (block.BlockRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.BlockRecord{}, false, err
	}
	rec, found, err := scanBlockRecord(tx.tx.QueryRow(ctx,
		`SELECT block_id, block_hash, length, live_chunk_count, sync_state
		 FROM block_records WHERE block_id = ?1`,
		blockID))
	if err != nil {
		return block.BlockRecord{}, false, fmt.Errorf("sqlite tx block_records get: %w", err)
	}
	return rec, found, nil
}

// DeleteBlockRecord removes the block record for blockID. Idempotent.
func (tx *sqliteTransaction) DeleteBlockRecord(ctx context.Context, blockID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := tx.tx.Exec(ctx,
		`DELETE FROM block_records WHERE block_id = ?1`,
		blockID); err != nil {
		return fmt.Errorf("sqlite tx block_records delete: %w", err)
	}
	return nil
}

// WalkBlockRecords calls fn for every stored block record.
func (tx *sqliteTransaction) WalkBlockRecords(ctx context.Context, fn func(block.BlockRecord) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rows, err := tx.tx.Query(ctx,
		`SELECT block_id, block_hash, length, live_chunk_count, sync_state FROM block_records`)
	if err != nil {
		return fmt.Errorf("sqlite tx block_records walk: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		rec, _, err := scanBlockRecordRow(rows)
		if err != nil {
			return fmt.Errorf("sqlite tx block_records walk scan: %w", err)
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	return rows.Err()
}

// DecrLiveChunkCount atomically decrements LiveChunkCount, flooring at 0.
// Returns the remaining count.  Errors if blockID does not exist.
func (tx *sqliteTransaction) DecrLiveChunkCount(ctx context.Context, blockID string, delta uint32) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Single atomic statement: MAX(0, ...) floors the count at zero in SQL,
	// avoiding a separate client-side SELECT and preventing lost updates when
	// multiple deferred transactions decrement the same row concurrently.
	// RETURNING live_chunk_count gives the post-update value without a second
	// round-trip.  ErrNoRows means the block does not exist.
	var remaining int64
	err := tx.tx.QueryRow(ctx,
		`UPDATE block_records
		 SET live_chunk_count = MAX(0, live_chunk_count - ?1)
		 WHERE block_id = ?2
		 RETURNING live_chunk_count`,
		int64(delta), blockID).Scan(&remaining)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("sqlite block_records decr: block %q not found", blockID)
	}
	if err != nil {
		return 0, fmt.Errorf("sqlite block_records decr: %w", err)
	}
	return uint32(remaining), nil
}

// ============================================================================
// Scan helpers
// ============================================================================

// scanBlockRecord scans a single-row query result into a BlockRecord.
// Returns (_, false, nil) on sql.ErrNoRows (miss is not an error).
func scanBlockRecord(row scanRow) (block.BlockRecord, bool, error) {
	var (
		blockID   string
		hashRaw   []byte
		length    int64
		liveCount uint32
		syncState int
	)
	err := row.Scan(&blockID, &hashRaw, &length, &liveCount, &syncState)
	if errors.Is(err, sql.ErrNoRows) {
		return block.BlockRecord{}, false, nil
	}
	if err != nil {
		return block.BlockRecord{}, false, err
	}
	if len(hashRaw) != len(block.ContentHash{}) {
		return block.BlockRecord{}, false, fmt.Errorf("sqlite scanBlockRecord: malformed block_hash length %d", len(hashRaw))
	}
	var h block.ContentHash
	copy(h[:], hashRaw)
	return block.BlockRecord{
		BlockID:        blockID,
		BlockHash:      h,
		Length:         length,
		LiveChunkCount: liveCount,
		SyncState:      block.BlockState(syncState),
	}, true, nil
}

// scanBlockRecordRow scans a streaming Rows cursor (from WalkBlockRecords).
// The caller drives rows.Next(); this only scans the current row.
// found is always true here since we are iterating an existing row set.
func scanBlockRecordRow(rows scanRows) (block.BlockRecord, bool, error) {
	var (
		blockID   string
		hashRaw   []byte
		length    int64
		liveCount uint32
		syncState int
	)
	if err := rows.Scan(&blockID, &hashRaw, &length, &liveCount, &syncState); err != nil {
		return block.BlockRecord{}, false, err
	}
	if len(hashRaw) != len(block.ContentHash{}) {
		return block.BlockRecord{}, false, fmt.Errorf("sqlite scanBlockRecordRow: malformed block_hash length %d", len(hashRaw))
	}
	var h block.ContentHash
	copy(h[:], hashRaw)
	return block.BlockRecord{
		BlockID:        blockID,
		BlockHash:      h,
		Length:         length,
		LiveChunkCount: liveCount,
		SyncState:      block.BlockState(syncState),
	}, true, nil
}
