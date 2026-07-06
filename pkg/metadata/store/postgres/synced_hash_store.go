// Per-CAS-hash idempotent presence marker backed by the synced_hashes
// table. See metadata.SyncedHashStore for the contract; this backend
// uses a tiny indexed table keyed by the raw 32-byte BLAKE3 hash, with
// ON CONFLICT DO NOTHING for idempotent MarkSynced and unconditional
// DELETE for idempotent DeleteSynced.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Compile-time assertions: the Postgres engine and its transaction implement
// SyncedHashStore.
var (
	_ metadata.SyncedHashStore = (*PostgresMetadataStore)(nil)
	_ metadata.SyncedHashStore = (*postgresTransaction)(nil)
)

// EnumerateSynced streams every synced marker with its locator and first-mirror
// time, read straight from the synced_hashes table. The locator columns live in
// the same row, so yielding them here lets callers resolve locators in a single
// scan instead of a GetLocator round trip per hash (#1554). A synced row with
// NULL/empty block columns yields the zero (standalone) locator.
func (s *PostgresMetadataStore) EnumerateSynced(ctx context.Context, fn func(hash block.ContentHash, loc block.ChunkLocator, syncedAt time.Time) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rows, err := s.query(ctx, `SELECT hash, synced_at, block_id, block_offset, block_length FROM synced_hashes`)
	if err != nil {
		return fmt.Errorf("postgres synced enumerate: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			raw      []byte
			syncedAt time.Time
			blockID  sql.NullString
			off, ln  sql.NullInt64
		)
		if err := rows.Scan(&raw, &syncedAt, &blockID, &off, &ln); err != nil {
			return fmt.Errorf("postgres synced enumerate scan: %w", err)
		}
		if len(raw) != len(block.ContentHash{}) {
			// Defensive: a malformed hash row cannot be reduced to a
			// ContentHash. Skip it rather than corrupt the sweep candidate.
			continue
		}
		var h block.ContentHash
		copy(h[:], raw)
		loc, err := locatorFromCols(blockID, off, ln)
		if err != nil {
			return fmt.Errorf("postgres synced enumerate: %w", err)
		}
		if err := fn(h, loc, syncedAt); err != nil {
			return err
		}
	}
	return rows.Err()
}

// locatorFromCols builds a ChunkLocator from already-scanned (block_id,
// block_offset, block_length) columns. NULL/empty block_id yields the zero
// (standalone) locator. Mirrors GetLocator's decode for the folded enumeration.
func locatorFromCols(blockID sql.NullString, off, length sql.NullInt64) (block.ChunkLocator, error) {
	if !blockID.Valid || blockID.String == "" {
		return block.ChunkLocator{}, nil
	}
	if !off.Valid || !length.Valid {
		return block.ChunkLocator{}, fmt.Errorf("corrupt locator row: block_id %q with NULL offset/length", blockID.String)
	}
	return block.ChunkLocator{BlockID: blockID.String, WireOffset: off.Int64, WireLength: length.Int64}, nil
}

// IsSynced reports whether hash has been MarkSynced'd at least once.
// Returns (false, nil) when no row exists for hash — an absent hash is
// treated as "not yet synced", not as an error.
func (s *PostgresMetadataStore) IsSynced(ctx context.Context, hash block.ContentHash) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	row := s.queryRow(ctx,
		`SELECT 1 FROM synced_hashes WHERE hash = $1`,
		hash[:])
	var dummy int
	err := row.Scan(&dummy)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("postgres synced get: %w", err)
	}
	return true, nil
}

// MarkSynced records that hash has been mirrored to remote, persisting loc's
// block columns atomically. Idempotent via ON CONFLICT (hash) DO NOTHING —
// re-applying the same hash is a no-op that preserves the first locator. A
// standalone locator (BlockID == "") leaves the block columns NULL, identical to
// a pre-locator row, so existing data needs no migration.
func (s *PostgresMetadataStore) MarkSynced(ctx context.Context, hash block.ContentHash, loc block.ChunkLocator) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	blockID, off, length := locatorArgs(loc)
	if _, err := s.exec(ctx,
		`INSERT INTO synced_hashes (hash, synced_at, block_id, block_offset, block_length)
			VALUES ($1, NOW(), $2, $3, $4)
			ON CONFLICT (hash) DO NOTHING`,
		hash[:], blockID, off, length); err != nil {
		return fmt.Errorf("postgres synced mark: %w", err)
	}
	return nil
}

// GetLocator returns the recorded remote locator for hash. (zero, false, nil)
// when no row exists; a synced row with NULL/empty block columns yields the zero
// (standalone) locator with found == true.
func (s *PostgresMetadataStore) GetLocator(ctx context.Context, hash block.ContentHash) (block.ChunkLocator, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.ChunkLocator{}, false, err
	}

	row := s.queryRow(ctx,
		`SELECT block_id, block_offset, block_length FROM synced_hashes WHERE hash = $1`,
		hash[:])
	var blockID sql.NullString
	var off, length sql.NullInt64
	err := row.Scan(&blockID, &off, &length)
	if errors.Is(err, pgx.ErrNoRows) {
		return block.ChunkLocator{}, false, nil
	}
	if err != nil {
		return block.ChunkLocator{}, false, fmt.Errorf("postgres synced get locator: %w", err)
	}
	if !blockID.Valid || blockID.String == "" {
		return block.ChunkLocator{}, true, nil
	}
	if !off.Valid || !length.Valid {
		return block.ChunkLocator{}, false, fmt.Errorf("corrupt locator row: block_id %q with NULL offset/length", blockID.String)
	}
	return block.ChunkLocator{BlockID: blockID.String, WireOffset: off.Int64, WireLength: length.Int64}, true, nil
}

// locatorArgs maps a ChunkLocator onto the (block_id, block_offset, block_length)
// INSERT args: NULL for a standalone chunk (so its row is identical to a
// pre-locator row), the recorded values for a block-resident chunk.
func locatorArgs(loc block.ChunkLocator) (blockID, off, length any) {
	if loc.IsStandalone() {
		return nil, nil, nil
	}
	return loc.BlockID, loc.WireOffset, loc.WireLength
}

// DeleteSynced removes the synced marker for hash. Idempotent: DELETE
// returns no error when the row does not exist (zero rows affected is
// not an error condition).
func (s *PostgresMetadataStore) DeleteSynced(ctx context.Context, hash block.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if _, err := s.exec(ctx,
		`DELETE FROM synced_hashes WHERE hash = $1`,
		hash[:]); err != nil {
		return fmt.Errorf("postgres synced delete: %w", err)
	}
	return nil
}

// ============================================================================
// Transaction-level SyncedHashStore
// ============================================================================
//
// Same executor plumbing as the transaction-level BlockRecordStore /
// LocalChunkIndex (block_record_store.go): each method runs its statement on
// the enclosing pgx.Tx (tx.tx) instead of the pool helpers, sharing the
// locatorArgs / scan logic with the store-level variants. Postgres gives
// read-your-writes within a transaction, so a MarkSynced after a DeleteSynced
// in the same tx records the new locator.

func (tx *postgresTransaction) IsSynced(ctx context.Context, hash block.ContentHash) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var dummy int
	err := tx.tx.QueryRow(ctx,
		`SELECT 1 FROM synced_hashes WHERE hash = $1`,
		hash[:]).Scan(&dummy)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("postgres tx synced get: %w", err)
	}
	return true, nil
}

// MarkSynced records the synced marker inside the transaction. First-wins per
// ON CONFLICT (hash) DO NOTHING, matching the store-level method — except
// after a DeleteSynced in the same tx, whose pending delete makes this insert
// take effect with the new locator (read-your-writes).
func (tx *postgresTransaction) MarkSynced(ctx context.Context, hash block.ContentHash, loc block.ChunkLocator) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	blockID, off, length := locatorArgs(loc)
	if _, err := tx.tx.Exec(ctx,
		`INSERT INTO synced_hashes (hash, synced_at, block_id, block_offset, block_length)
			VALUES ($1, NOW(), $2, $3, $4)
			ON CONFLICT (hash) DO NOTHING`,
		hash[:], blockID, off, length); err != nil {
		return fmt.Errorf("postgres tx synced mark: %w", err)
	}
	return nil
}

func (tx *postgresTransaction) GetLocator(ctx context.Context, hash block.ContentHash) (block.ChunkLocator, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.ChunkLocator{}, false, err
	}
	var blockID sql.NullString
	var off, length sql.NullInt64
	err := tx.tx.QueryRow(ctx,
		`SELECT block_id, block_offset, block_length FROM synced_hashes WHERE hash = $1`,
		hash[:]).Scan(&blockID, &off, &length)
	if errors.Is(err, pgx.ErrNoRows) {
		return block.ChunkLocator{}, false, nil
	}
	if err != nil {
		return block.ChunkLocator{}, false, fmt.Errorf("postgres tx synced get locator: %w", err)
	}
	if !blockID.Valid || blockID.String == "" {
		return block.ChunkLocator{}, true, nil
	}
	if !off.Valid || !length.Valid {
		return block.ChunkLocator{}, false, fmt.Errorf("corrupt locator row: block_id %q with NULL offset/length", blockID.String)
	}
	return block.ChunkLocator{BlockID: blockID.String, WireOffset: off.Int64, WireLength: length.Int64}, true, nil
}

func (tx *postgresTransaction) DeleteSynced(ctx context.Context, hash block.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := tx.tx.Exec(ctx,
		`DELETE FROM synced_hashes WHERE hash = $1`,
		hash[:]); err != nil {
		return fmt.Errorf("postgres tx synced delete: %w", err)
	}
	return nil
}
