// Block-record implementation for the PostgreSQL metadata backend. See
// metadata.BlockRecordStore for the contract; pkg/metadata/store/memory/ is the
// canonical semantic reference.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Compile-time assertions: the store and its transaction both satisfy the
// interface.
var _ metadata.BlockRecordStore = (*PostgresMetadataStore)(nil)
var _ metadata.BlockRecordStore = (*postgresTransaction)(nil)

// ============================================================================
// Transaction-level BlockRecordStore
// ============================================================================

func (tx *postgresTransaction) PutBlockRecord(ctx context.Context, rec block.BlockRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := tx.tx.Exec(ctx, `
		INSERT INTO block_records (block_id, block_hash, length, live_chunk_count, sync_state)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (block_id) DO UPDATE SET
			block_hash       = EXCLUDED.block_hash,
			length           = EXCLUDED.length,
			live_chunk_count = EXCLUDED.live_chunk_count,
			sync_state       = EXCLUDED.sync_state`,
		rec.BlockID, rec.BlockHash[:], rec.Length, rec.LiveChunkCount, int16(rec.SyncState),
	)
	if err != nil {
		return fmt.Errorf("postgres PutBlockRecord: %w", err)
	}
	return nil
}

func (tx *postgresTransaction) GetBlockRecord(ctx context.Context, blockID string) (block.BlockRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.BlockRecord{}, false, err
	}
	return scanBlockRecord(tx.tx.QueryRow(ctx,
		`SELECT block_id, block_hash, length, live_chunk_count, sync_state
		 FROM block_records WHERE block_id = $1`,
		blockID,
	))
}

func (tx *postgresTransaction) DeleteBlockRecord(ctx context.Context, blockID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := tx.tx.Exec(ctx, `DELETE FROM block_records WHERE block_id = $1`, blockID)
	if err != nil {
		return fmt.Errorf("postgres DeleteBlockRecord: %w", err)
	}
	return nil
}

func (tx *postgresTransaction) WalkBlockRecords(ctx context.Context, fn func(block.BlockRecord) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rows, err := tx.tx.Query(ctx,
		`SELECT block_id, block_hash, length, live_chunk_count, sync_state FROM block_records`,
	)
	if err != nil {
		return fmt.Errorf("postgres WalkBlockRecords: %w", err)
	}
	defer rows.Close()
	return iterBlockRecordRows(rows, fn)
}

// DecrLiveChunkCount atomically floors live_chunk_count at 0.
// Returns an error if blockID does not exist (matches memory semantics).
func (tx *postgresTransaction) DecrLiveChunkCount(ctx context.Context, blockID string, delta uint32) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var remaining int64
	err := tx.tx.QueryRow(ctx, `
		UPDATE block_records
		SET live_chunk_count = GREATEST(0, live_chunk_count - $2)
		WHERE block_id = $1
		RETURNING live_chunk_count`,
		blockID, int64(delta),
	).Scan(&remaining)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("block record %q not found", blockID)
	}
	if err != nil {
		return 0, fmt.Errorf("postgres DecrLiveChunkCount: %w", err)
	}
	return uint32(remaining), nil
}

// ============================================================================
// Store-level BlockRecordStore (delegates writes through WithTransaction)
// ============================================================================

func (s *PostgresMetadataStore) PutBlockRecord(ctx context.Context, rec block.BlockRecord) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutBlockRecord(ctx, rec)
	})
}

func (s *PostgresMetadataStore) GetBlockRecord(ctx context.Context, blockID string) (block.BlockRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.BlockRecord{}, false, err
	}
	return scanBlockRecord(s.queryRow(ctx,
		`SELECT block_id, block_hash, length, live_chunk_count, sync_state
		 FROM block_records WHERE block_id = $1`,
		blockID,
	))
}

func (s *PostgresMetadataStore) DeleteBlockRecord(ctx context.Context, blockID string) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteBlockRecord(ctx, blockID)
	})
}

func (s *PostgresMetadataStore) WalkBlockRecords(ctx context.Context, fn func(block.BlockRecord) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rows, err := s.query(ctx,
		`SELECT block_id, block_hash, length, live_chunk_count, sync_state FROM block_records`,
	)
	if err != nil {
		return fmt.Errorf("postgres WalkBlockRecords: %w", err)
	}
	defer rows.Close()
	return iterBlockRecordRows(rows, fn)
}

func (s *PostgresMetadataStore) DecrLiveChunkCount(ctx context.Context, blockID string, delta uint32) (uint32, error) {
	var remaining uint32
	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		remaining, err = tx.DecrLiveChunkCount(ctx, blockID, delta)
		return err
	})
	return remaining, err
}

// ============================================================================
// CommitBlock
// ============================================================================

// CommitBlock atomically writes rec within a single transaction, then marks
// each chunk synced. Delegates to DefaultCommitBlock for idempotency logic —
// identical to the memory and badger backends.
func (s *PostgresMetadataStore) CommitBlock(ctx context.Context, rec block.BlockRecord, chunks []block.BlockChunkCommit) error {
	return metadata.DefaultCommitBlock(ctx, s, rec, chunks, nil)
}

// ============================================================================
// Shared row-scan helpers
// ============================================================================

// scanBlockRecord reads a BlockRecord from a single pgx.Row (or pgx.Rows
// result reused as a row). Returns (_, false, nil) on a missing row.
func scanBlockRecord(row pgx.Row) (block.BlockRecord, bool, error) {
	var (
		blockID        string
		blockHashRaw   []byte
		length         int64
		liveChunkCount int64
		syncState      int16
	)
	err := row.Scan(&blockID, &blockHashRaw, &length, &liveChunkCount, &syncState)
	if errors.Is(err, pgx.ErrNoRows) {
		return block.BlockRecord{}, false, nil
	}
	if err != nil {
		return block.BlockRecord{}, false, fmt.Errorf("postgres scanBlockRecord: %w", err)
	}
	if len(blockHashRaw) != len(block.ContentHash{}) {
		return block.BlockRecord{}, false, fmt.Errorf("postgres scanBlockRecord: malformed block_hash length %d", len(blockHashRaw))
	}
	var h block.ContentHash
	copy(h[:], blockHashRaw)
	return block.BlockRecord{
		BlockID:        blockID,
		BlockHash:      h,
		Length:         length,
		LiveChunkCount: uint32(liveChunkCount),
		SyncState:      block.BlockState(syncState),
	}, true, nil
}

// iterBlockRecordRows calls fn for every row in rows, returning the first error.
func iterBlockRecordRows(rows pgx.Rows, fn func(block.BlockRecord) error) error {
	for rows.Next() {
		rec, ok, err := scanBlockRecord(rows)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	return rows.Err()
}
