// Block-record and local-chunk-index implementation for the PostgreSQL
// metadata backend. See metadata.BlockRecordStore and metadata.LocalChunkIndex
// for the contracts; pkg/metadata/store/memory/ is the canonical semantic
// reference.
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
// interfaces.
var _ metadata.BlockRecordStore = (*PostgresMetadataStore)(nil)
var _ metadata.LocalChunkIndex = (*PostgresMetadataStore)(nil)
var _ metadata.BlockRecordStore = (*postgresTransaction)(nil)
var _ metadata.LocalChunkIndex = (*postgresTransaction)(nil)

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
// Transaction-level LocalChunkIndex
// ============================================================================

func (tx *postgresTransaction) PutLocalLocation(ctx context.Context, hash block.ContentHash, loc block.LocalChunkLocation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := tx.tx.Exec(ctx, `
		INSERT INTO local_chunk_index (hash, log_blob_id, raw_offset, raw_length)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (hash) DO UPDATE SET
			log_blob_id = EXCLUDED.log_blob_id,
			raw_offset  = EXCLUDED.raw_offset,
			raw_length  = EXCLUDED.raw_length`,
		hash[:], loc.LogBlobID, loc.RawOffset, loc.RawLength,
	)
	if err != nil {
		return fmt.Errorf("postgres PutLocalLocation: %w", err)
	}
	return nil
}

func (tx *postgresTransaction) GetLocalLocation(ctx context.Context, hash block.ContentHash) (block.LocalChunkLocation, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.LocalChunkLocation{}, false, err
	}
	return scanLocalLocation(tx.tx.QueryRow(ctx,
		`SELECT log_blob_id, raw_offset, raw_length FROM local_chunk_index WHERE hash = $1`,
		hash[:],
	))
}

func (tx *postgresTransaction) DeleteLocalLocation(ctx context.Context, hash block.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := tx.tx.Exec(ctx, `DELETE FROM local_chunk_index WHERE hash = $1`, hash[:])
	if err != nil {
		return fmt.Errorf("postgres DeleteLocalLocation: %w", err)
	}
	return nil
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
// Store-level LocalChunkIndex
// ============================================================================

func (s *PostgresMetadataStore) PutLocalLocation(ctx context.Context, hash block.ContentHash, loc block.LocalChunkLocation) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutLocalLocation(ctx, hash, loc)
	})
}

func (s *PostgresMetadataStore) GetLocalLocation(ctx context.Context, hash block.ContentHash) (block.LocalChunkLocation, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.LocalChunkLocation{}, false, err
	}
	return scanLocalLocation(s.queryRow(ctx,
		`SELECT log_blob_id, raw_offset, raw_length FROM local_chunk_index WHERE hash = $1`,
		hash[:],
	))
}

func (s *PostgresMetadataStore) DeleteLocalLocation(ctx context.Context, hash block.ContentHash) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteLocalLocation(ctx, hash)
	})
}

// WalkLocalLocations calls fn for every stored content-hash -> log-blob
// location. Read-only; deliberately NOT part of the LocalChunkIndex interface —
// the local block store discovers it via a narrow consumer-side interface to
// re-seed crash-stranded unsynced logblob chunks on restart (Walk / ListUnsynced).
func (s *PostgresMetadataStore) WalkLocalLocations(ctx context.Context, fn func(block.ContentHash, block.LocalChunkLocation) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rows, err := s.query(ctx,
		`SELECT hash, log_blob_id, raw_offset, raw_length FROM local_chunk_index`)
	if err != nil {
		return fmt.Errorf("postgres WalkLocalLocations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		// The local chunk index can be very large (one row per unique chunk),
		// so re-check cancellation per row to stay responsive during a full walk.
		if err := ctx.Err(); err != nil {
			return err
		}
		var (
			hashRaw   []byte
			logBlobID string
			rawOffset int64
			rawLength int64
		)
		if err := rows.Scan(&hashRaw, &logBlobID, &rawOffset, &rawLength); err != nil {
			return fmt.Errorf("postgres WalkLocalLocations scan: %w", err)
		}
		if len(hashRaw) != len(block.ContentHash{}) {
			return fmt.Errorf("postgres WalkLocalLocations: malformed hash length %d", len(hashRaw))
		}
		var h block.ContentHash
		copy(h[:], hashRaw)
		if err := fn(h, block.LocalChunkLocation{
			LogBlobID: logBlobID,
			RawOffset: rawOffset,
			RawLength: rawLength,
		}); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ============================================================================
// CommitBlock
// ============================================================================

// CommitBlock atomically writes rec and all chunk local locations within a
// single transaction, then marks each chunk synced outside the transaction.
// Delegates to DefaultCommitBlock for idempotency logic — identical to the
// memory and badger backends.
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

// scanLocalLocation reads a LocalChunkLocation from a single row.
// Returns (_, false, nil) on a missing row.
func scanLocalLocation(row pgx.Row) (block.LocalChunkLocation, bool, error) {
	var (
		logBlobID string
		rawOffset int64
		rawLength int64
	)
	err := row.Scan(&logBlobID, &rawOffset, &rawLength)
	if errors.Is(err, pgx.ErrNoRows) {
		return block.LocalChunkLocation{}, false, nil
	}
	if err != nil {
		return block.LocalChunkLocation{}, false, fmt.Errorf("postgres scanLocalLocation: %w", err)
	}
	return block.LocalChunkLocation{
		LogBlobID: logBlobID,
		RawOffset: rawOffset,
		RawLength: rawLength,
	}, true, nil
}
