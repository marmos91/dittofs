// BlockRecordStore + LocalChunkIndex implementations for the SQLite metadata
// store.  Both interfaces are required by metadata.Transaction, so every method
// exists in two variants: a store-level (pool path) variant on
// *SQLiteMetadataStore and a transaction-scoped variant on *sqliteTransaction.
//
// Semantics match the memory backend exactly:
//   - PutBlockRecord / PutLocalLocation — idempotent upserts.
//   - GetBlockRecord / GetLocalLocation — (_, false, nil) on miss.
//   - DeleteBlockRecord / DeleteLocalLocation — idempotent (missing row → nil).
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

// Compile-time assertions: the store and its transaction both satisfy the new
// interfaces.  If a method signature drifts these lines will fail to compile
// before any test runs.
var _ metadata.BlockRecordStore = (*SQLiteMetadataStore)(nil)
var _ metadata.LocalChunkIndex = (*SQLiteMetadataStore)(nil)
var _ metadata.BlockRecordStore = (*sqliteTransaction)(nil)
var _ metadata.LocalChunkIndex = (*sqliteTransaction)(nil)

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
// Store-level LocalChunkIndex (pool path)
// ============================================================================

// PutLocalLocation records or overwrites the local position for hash.
func (s *SQLiteMetadataStore) PutLocalLocation(ctx context.Context, hash block.ContentHash, loc block.LocalChunkLocation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := s.exec(ctx,
		`INSERT INTO local_chunk_index (hash, log_blob_id, raw_offset, raw_length)
		 VALUES (?1, ?2, ?3, ?4)
		 ON CONFLICT (hash) DO UPDATE SET
		     log_blob_id = EXCLUDED.log_blob_id,
		     raw_offset  = EXCLUDED.raw_offset,
		     raw_length  = EXCLUDED.raw_length`,
		hash[:], loc.LogBlobID, loc.RawOffset, loc.RawLength)
	if err != nil {
		return fmt.Errorf("sqlite local_chunk_index put: %w", err)
	}
	return nil
}

// GetLocalLocation returns the local position for hash.
// Returns (_, false, nil) when no entry exists.
func (s *SQLiteMetadataStore) GetLocalLocation(ctx context.Context, hash block.ContentHash) (block.LocalChunkLocation, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.LocalChunkLocation{}, false, err
	}
	loc, found, err := scanLocalLocation(s.queryRow(ctx,
		`SELECT log_blob_id, raw_offset, raw_length FROM local_chunk_index WHERE hash = ?1`,
		hash[:]))
	if err != nil {
		return block.LocalChunkLocation{}, false, fmt.Errorf("sqlite local_chunk_index get: %w", err)
	}
	return loc, found, nil
}

// DeleteLocalLocation removes the local position for hash. Idempotent.
func (s *SQLiteMetadataStore) DeleteLocalLocation(ctx context.Context, hash block.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.exec(ctx,
		`DELETE FROM local_chunk_index WHERE hash = ?1`,
		hash[:]); err != nil {
		return fmt.Errorf("sqlite local_chunk_index delete: %w", err)
	}
	return nil
}

// WalkLocalLocations calls fn for every stored content-hash -> log-blob
// location. Read-only; deliberately NOT part of the LocalChunkIndex interface —
// the local block store discovers it via a narrow consumer-side interface to
// re-seed crash-stranded unsynced logblob chunks on restart (Walk / ListUnsynced).
// Streams rows on the pool path, mirroring WalkBlockRecords.
func (s *SQLiteMetadataStore) WalkLocalLocations(ctx context.Context, fn func(block.ContentHash, block.LocalChunkLocation) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rows, err := s.query(ctx,
		`SELECT hash, log_blob_id, raw_offset, raw_length FROM local_chunk_index`)
	if err != nil {
		return fmt.Errorf("sqlite local_chunk_index walk: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		// The local chunk index can be very large (one row per unique chunk),
		// so re-check cancellation per row to stay responsive during a full walk.
		if err := ctx.Err(); err != nil {
			return err
		}
		h, loc, err := scanLocalLocationRow(rows)
		if err != nil {
			return fmt.Errorf("sqlite local_chunk_index walk scan: %w", err)
		}
		if err := fn(h, loc); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ============================================================================
// CommitBlock (store-level, delegates to DefaultCommitBlock)
// ============================================================================

// CommitBlock atomically writes rec and all chunk local locations within a
// single transaction, then marks each chunk synced.  Idempotent on BlockID.
func (s *SQLiteMetadataStore) CommitBlock(ctx context.Context, rec block.BlockRecord, chunks []block.BlockChunkCommit) error {
	return metadata.DefaultCommitBlock(ctx, s, rec, chunks)
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
// Transaction-level LocalChunkIndex
// ============================================================================

// PutLocalLocation records or overwrites the local position for hash.
func (tx *sqliteTransaction) PutLocalLocation(ctx context.Context, hash block.ContentHash, loc block.LocalChunkLocation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := tx.tx.Exec(ctx,
		`INSERT INTO local_chunk_index (hash, log_blob_id, raw_offset, raw_length)
		 VALUES (?1, ?2, ?3, ?4)
		 ON CONFLICT (hash) DO UPDATE SET
		     log_blob_id = EXCLUDED.log_blob_id,
		     raw_offset  = EXCLUDED.raw_offset,
		     raw_length  = EXCLUDED.raw_length`,
		hash[:], loc.LogBlobID, loc.RawOffset, loc.RawLength)
	if err != nil {
		return fmt.Errorf("sqlite tx local_chunk_index put: %w", err)
	}
	return nil
}

// GetLocalLocation returns the local position for hash.
// Returns (_, false, nil) when no entry exists.
func (tx *sqliteTransaction) GetLocalLocation(ctx context.Context, hash block.ContentHash) (block.LocalChunkLocation, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.LocalChunkLocation{}, false, err
	}
	loc, found, err := scanLocalLocation(tx.tx.QueryRow(ctx,
		`SELECT log_blob_id, raw_offset, raw_length FROM local_chunk_index WHERE hash = ?1`,
		hash[:]))
	if err != nil {
		return block.LocalChunkLocation{}, false, fmt.Errorf("sqlite tx local_chunk_index get: %w", err)
	}
	return loc, found, nil
}

// DeleteLocalLocation removes the local position for hash. Idempotent.
func (tx *sqliteTransaction) DeleteLocalLocation(ctx context.Context, hash block.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := tx.tx.Exec(ctx,
		`DELETE FROM local_chunk_index WHERE hash = ?1`,
		hash[:]); err != nil {
		return fmt.Errorf("sqlite tx local_chunk_index delete: %w", err)
	}
	return nil
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

// scanLocalLocationRow scans a streaming Rows cursor (from WalkLocalLocations),
// including the hash key column. The caller drives rows.Next().
func scanLocalLocationRow(rows scanRows) (block.ContentHash, block.LocalChunkLocation, error) {
	var (
		hashRaw   []byte
		logBlobID string
		rawOffset int64
		rawLength int64
	)
	if err := rows.Scan(&hashRaw, &logBlobID, &rawOffset, &rawLength); err != nil {
		return block.ContentHash{}, block.LocalChunkLocation{}, err
	}
	if len(hashRaw) != len(block.ContentHash{}) {
		return block.ContentHash{}, block.LocalChunkLocation{}, fmt.Errorf("sqlite scanLocalLocationRow: malformed hash length %d", len(hashRaw))
	}
	var h block.ContentHash
	copy(h[:], hashRaw)
	return h, block.LocalChunkLocation{
		LogBlobID: logBlobID,
		RawOffset: rawOffset,
		RawLength: rawLength,
	}, nil
}

// scanLocalLocation scans a single-row query result into a LocalChunkLocation.
// Returns (_, false, nil) on sql.ErrNoRows.
func scanLocalLocation(row scanRow) (block.LocalChunkLocation, bool, error) {
	var (
		logBlobID string
		rawOffset int64
		rawLength int64
	)
	err := row.Scan(&logBlobID, &rawOffset, &rawLength)
	if errors.Is(err, sql.ErrNoRows) {
		return block.LocalChunkLocation{}, false, nil
	}
	if err != nil {
		return block.LocalChunkLocation{}, false, err
	}
	return block.LocalChunkLocation{
		LogBlobID: logBlobID,
		RawOffset: rawOffset,
		RawLength: rawLength,
	}, true, nil
}
