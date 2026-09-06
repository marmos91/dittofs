package sql

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// BlockRecordColumns is the block_records column list every read names, in the
// order scanBlockRecord reads them and PutBlockRecord supplies them. Naming it
// once is what keeps a column added to one statement from being missed by the
// others, or by the scan.
const BlockRecordColumns = `block_id, block_hash, length, live_chunk_count, sync_state`

// walkBlockRecords carries no placeholder, so both dialects spell it
// identically and it stays here rather than in BlockRecordQueries.
const walkBlockRecords = `SELECT ` + BlockRecordColumns + ` FROM block_records`

// BlockRecordQueries holds the block-record statements that differ between the
// dialects. Every difference here is placeholder syntax except Decr, where the
// floor-at-zero function is spelled MAX on sqlite and GREATEST on postgres.
type BlockRecordQueries struct {
	// Put upserts one record. Five parameters, in BlockRecordColumns order.
	Put string
	// SelectByID reads one record. One parameter: the block id.
	SelectByID string
	// Delete removes one record. One parameter: the block id.
	Delete string
	// Decr subtracts from live_chunk_count, flooring at zero, and returns the
	// remaining count. Two parameters: the delta, then the block id. It must
	// stay a single statement — that is what makes the read-modify-write
	// atomic without a surrounding transaction.
	Decr string
}

// Core serves the block-record surface on both the pool and an open
// transaction. Neither path keeps transaction-local state for these rows, so
// unlike the share writes there is no shadow method on either side.
var _ metadata.BlockRecordStore = (*Core)(nil)

// PutBlockRecord writes or overwrites the block record for rec.BlockID.
func (c *Core) PutBlockRecord(ctx context.Context, rec block.BlockRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// sync_state binds as int16 because postgres declares it SMALLINT and pgx
	// refuses a wider integer for it; database/sql widens int16 for sqlite.
	if _, err := c.X.Exec(ctx, c.D.BlockRecords().Put,
		rec.BlockID, rec.BlockHash[:], rec.Length, rec.LiveChunkCount, int16(rec.SyncState),
	); err != nil {
		return fmt.Errorf("block_records put %q: %w", rec.BlockID, err)
	}
	return nil
}

// GetBlockRecord retrieves the block record for blockID, reporting
// (_, false, nil) when no record exists.
func (c *Core) GetBlockRecord(ctx context.Context, blockID string) (block.BlockRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.BlockRecord{}, false, err
	}
	rec, err := scanBlockRecord(c.X.QueryRow(ctx, c.D.BlockRecords().SelectByID, blockID))
	if c.D.IsNoRows(err) {
		return block.BlockRecord{}, false, nil
	}
	if err != nil {
		return block.BlockRecord{}, false, fmt.Errorf("block_records get %q: %w", blockID, err)
	}
	return rec, true, nil
}

// DeleteBlockRecord removes the block record for blockID. Deleting an absent
// record is not an error.
func (c *Core) DeleteBlockRecord(ctx context.Context, blockID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := c.X.Exec(ctx, c.D.BlockRecords().Delete, blockID); err != nil {
		return fmt.Errorf("block_records delete %q: %w", blockID, err)
	}
	return nil
}

// WalkBlockRecords calls fn for every stored block record, stopping at the
// first error from fn or from the iterator.
func (c *Core) WalkBlockRecords(ctx context.Context, fn func(block.BlockRecord) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rows, err := c.X.Query(ctx, walkBlockRecords)
	if err != nil {
		return fmt.Errorf("block_records walk: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		rec, err := scanBlockRecord(rows)
		if err != nil {
			return fmt.Errorf("block_records walk scan: %w", err)
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	return rows.Err()
}

// DecrLiveChunkCount subtracts delta from blockID's live chunk count, flooring
// at zero, and returns what remains. An absent block id is an error: the caller
// is decrementing a count it believes exists, and silently reporting zero would
// hide a block record that was reaped early.
func (c *Core) DecrLiveChunkCount(ctx context.Context, blockID string, delta uint32) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var remaining int64
	err := c.X.QueryRow(ctx, c.D.BlockRecords().Decr, delta, blockID).Scan(&remaining)
	if c.D.IsNoRows(err) {
		return 0, fmt.Errorf("block record %q not found", blockID)
	}
	if err != nil {
		return 0, fmt.Errorf("block_records decrement %q: %w", blockID, err)
	}
	return uint32(remaining), nil
}

// scanBlockRecord reads one block-record row.
//
// The scan targets are the widest type each dialect can hand back: postgres
// declares length and live_chunk_count BIGINT and sync_state SMALLINT, and pgx
// will not narrow either on the caller's behalf.
func scanBlockRecord(row Row) (block.BlockRecord, error) {
	var (
		blockID        string
		blockHash      []byte
		length         int64
		liveChunkCount int64
		syncState      int16
	)
	if err := row.Scan(&blockID, &blockHash, &length, &liveChunkCount, &syncState); err != nil {
		return block.BlockRecord{}, err
	}
	if len(blockHash) != len(block.ContentHash{}) {
		return block.BlockRecord{},
			fmt.Errorf("malformed block_hash length %d for block %q", len(blockHash), blockID)
	}
	var h block.ContentHash
	copy(h[:], blockHash)
	return block.BlockRecord{
		BlockID:        blockID,
		BlockHash:      h,
		Length:         length,
		LiveChunkCount: uint32(liveChunkCount),
		SyncState:      block.BlockState(syncState),
	}, nil
}
