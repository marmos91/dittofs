// Per-CAS-hash idempotent presence marker backed by the synced_hashes
// table. See metadata.SyncedHashStore for the contract; this backend
// uses a tiny indexed table keyed by the raw 32-byte BLAKE3 hash, with
// ON CONFLICT DO NOTHING for idempotent MarkSynced and unconditional
// DELETE for idempotent DeleteSynced.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Compile-time assertions: the SQLite engine and its transaction implement
// SyncedHashStore.
var (
	_ metadata.SyncedHashStore = (*SQLiteMetadataStore)(nil)
	_ metadata.SyncedHashStore = (*sqliteTransaction)(nil)
)

// EnumerateSynced streams every synced marker with its locator and first-mirror
// time, read straight from the synced_hashes table. The locator columns live in
// the same row, so yielding them here lets callers resolve locators in a single
// scan instead of a GetLocator round trip per hash — the O(N)-serial-statements
// cost on the MaxOpenConns(1) pool behind the slow cold-start (#1554). A synced
// row with NULL/empty block columns yields the zero (standalone) locator.
func (s *SQLiteMetadataStore) EnumerateSynced(ctx context.Context, fn func(hash block.ContentHash, loc block.ChunkLocator, syncedAt time.Time) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rows, err := s.query(ctx, `SELECT hash, synced_at, block_id, block_offset, block_length FROM synced_hashes`)
	if err != nil {
		return fmt.Errorf("sqlite synced enumerate: %w", err)
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
			return fmt.Errorf("sqlite synced enumerate scan: %w", err)
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
			return fmt.Errorf("sqlite synced enumerate: %w", err)
		}
		if err := fn(h, loc, syncedAt); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ============================================================================
// Shared bodies (executor-parameterized)
// ============================================================================
//
// Both the store-level (pool) path and the transaction path present the same
// pgx-shaped execer surface (see pool_helpers.go), so — unlike the ported
// Postgres file — each query body exists exactly once, parameterized on the
// executor. Within a transaction SQLite gives read-your-writes, so a
// MarkSynced after a DeleteSynced in the same tx records the new locator.

// syncedIsSynced reports marker presence via x.
func syncedIsSynced(ctx context.Context, x execer, hash block.ContentHash) (bool, error) {
	row := x.QueryRow(ctx,
		`SELECT 1 FROM synced_hashes WHERE hash = ?1`,
		hash[:])
	var dummy int
	err := row.Scan(&dummy)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqlite synced get: %w", err)
	}
	return true, nil
}

// syncedMark inserts the marker via x. First-wins per ON CONFLICT DO NOTHING.
func syncedMark(ctx context.Context, x execer, hash block.ContentHash, loc block.ChunkLocator) error {
	blockID, off, length := locatorArgs(loc)
	if _, err := x.Exec(ctx,
		`INSERT INTO synced_hashes (hash, synced_at, block_id, block_offset, block_length)
			VALUES (?1, CURRENT_TIMESTAMP, ?2, ?3, ?4)
			ON CONFLICT (hash) DO NOTHING`,
		hash[:], blockID, off, length); err != nil {
		return fmt.Errorf("sqlite synced mark: %w", err)
	}
	return nil
}

// syncedGetLocator reads the marker's locator via x.
func syncedGetLocator(ctx context.Context, x execer, hash block.ContentHash) (block.ChunkLocator, bool, error) {
	row := x.QueryRow(ctx,
		`SELECT block_id, block_offset, block_length FROM synced_hashes WHERE hash = ?1`,
		hash[:])
	loc, err := scanLocatorRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return block.ChunkLocator{}, false, nil
	}
	if err != nil {
		return block.ChunkLocator{}, false, fmt.Errorf("sqlite synced get locator: %w", err)
	}
	return loc, true, nil
}

// syncedDelete removes the marker via x. Idempotent.
func syncedDelete(ctx context.Context, x execer, hash block.ContentHash) error {
	if _, err := x.Exec(ctx,
		`DELETE FROM synced_hashes WHERE hash = ?1`,
		hash[:]); err != nil {
		return fmt.Errorf("sqlite synced delete: %w", err)
	}
	return nil
}

// IsSynced reports whether hash has been MarkSynced'd at least once.
// Returns (false, nil) when no row exists for hash — an absent hash is
// treated as "not yet synced", not as an error.
func (s *SQLiteMetadataStore) IsSynced(ctx context.Context, hash block.ContentHash) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return syncedIsSynced(ctx, s.conn(), hash)
}

// MarkSynced records that hash has been mirrored to remote, persisting loc's
// block columns atomically. Idempotent via ON CONFLICT (hash) DO NOTHING —
// re-applying the same hash is a no-op that preserves the first locator. A
// standalone locator (BlockID == "") leaves the block columns NULL, identical to
// a pre-locator row, so existing data needs no migration.
func (s *SQLiteMetadataStore) MarkSynced(ctx context.Context, hash block.ContentHash, loc block.ChunkLocator) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return syncedMark(ctx, s.conn(), hash, loc)
}

// GetLocator returns the recorded remote locator for hash. (zero, false, nil)
// when no row exists; a synced row with NULL/empty block columns yields the zero
// (standalone) locator with found == true.
func (s *SQLiteMetadataStore) GetLocator(ctx context.Context, hash block.ContentHash) (block.ChunkLocator, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.ChunkLocator{}, false, err
	}
	return syncedGetLocator(ctx, s.conn(), hash)
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

// scanLocatorRow scans a (block_id, block_offset, block_length) row into a
// ChunkLocator. NULL/empty block_id yields the zero (standalone) locator.
func scanLocatorRow(row scanRow) (block.ChunkLocator, error) {
	var blockID sql.NullString
	var off, length sql.NullInt64
	if err := row.Scan(&blockID, &off, &length); err != nil {
		return block.ChunkLocator{}, err
	}
	return locatorFromCols(blockID, off, length)
}

// locatorFromCols builds a ChunkLocator from already-scanned (block_id,
// block_offset, block_length) columns. NULL/empty block_id yields the zero
// (standalone) locator. Shared by scanLocatorRow (single-row lookup) and
// EnumerateSynced (folded into the enumeration scan).
func locatorFromCols(blockID sql.NullString, off, length sql.NullInt64) (block.ChunkLocator, error) {
	if !blockID.Valid || blockID.String == "" {
		return block.ChunkLocator{}, nil
	}
	if !off.Valid || !length.Valid {
		return block.ChunkLocator{}, fmt.Errorf("corrupt locator row: block_id %q with NULL offset/length", blockID.String)
	}
	return block.ChunkLocator{BlockID: blockID.String, WireOffset: off.Int64, WireLength: length.Int64}, nil
}

// DeleteSynced removes the synced marker for hash. Idempotent: DELETE
// returns no error when the row does not exist (zero rows affected is
// not an error condition).
func (s *SQLiteMetadataStore) DeleteSynced(ctx context.Context, hash block.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return syncedDelete(ctx, s.conn(), hash)
}

// ============================================================================
// Transaction-level SyncedHashStore
// ============================================================================
//
// Same executor plumbing as the transaction-level BlockRecordStore /
// LocalChunkIndex (block_record_store.go): run the shared bodies against the
// enclosing transaction's execer (tx.tx).

func (tx *sqliteTransaction) IsSynced(ctx context.Context, hash block.ContentHash) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return syncedIsSynced(ctx, tx.tx, hash)
}

func (tx *sqliteTransaction) MarkSynced(ctx context.Context, hash block.ContentHash, loc block.ChunkLocator) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return syncedMark(ctx, tx.tx, hash, loc)
}

func (tx *sqliteTransaction) GetLocator(ctx context.Context, hash block.ContentHash) (block.ChunkLocator, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.ChunkLocator{}, false, err
	}
	return syncedGetLocator(ctx, tx.tx, hash)
}

func (tx *sqliteTransaction) DeleteSynced(ctx context.Context, hash block.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return syncedDelete(ctx, tx.tx, hash)
}
