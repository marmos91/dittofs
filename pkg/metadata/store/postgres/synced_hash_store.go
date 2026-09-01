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

	storesql "github.com/marmos91/dittofs/pkg/metadata/store/sql"
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
	return isSyncedTx(ctx, s.conn(), "postgres", hash)
}

// MarkSynced records that hash has been mirrored to remote, persisting loc's
// block columns atomically.
func (s *PostgresMetadataStore) MarkSynced(ctx context.Context, hash block.ContentHash, loc block.ChunkLocator) error {
	return markSyncedTx(ctx, s.conn(), "postgres", hash, loc)
}

// GetLocator returns the recorded remote locator for hash.
func (s *PostgresMetadataStore) GetLocator(ctx context.Context, hash block.ContentHash) (block.ChunkLocator, bool, error) {
	return getLocatorTx(ctx, s.conn(), "postgres", hash)
}

// DeleteSynced removes the synced marker for hash.
func (s *PostgresMetadataStore) DeleteSynced(ctx context.Context, hash block.ContentHash) error {
	return deleteSyncedTx(ctx, s.conn(), "postgres", hash)
}

// ============================================================================
// Transaction-level SyncedHashStore
// ============================================================================
//
// The transaction variants run the same bodies against the enclosing pgx.Tx
// instead of the pool. Postgres gives read-your-writes within a transaction, so
// a MarkSynced after a DeleteSynced in the same tx records the new locator.

func (tx *postgresTransaction) IsSynced(ctx context.Context, hash block.ContentHash) (bool, error) {
	return isSyncedTx(ctx, tx.conn(), "postgres tx", hash)
}

func (tx *postgresTransaction) MarkSynced(ctx context.Context, hash block.ContentHash, loc block.ChunkLocator) error {
	return markSyncedTx(ctx, tx.conn(), "postgres tx", hash, loc)
}

func (tx *postgresTransaction) GetLocator(ctx context.Context, hash block.ContentHash) (block.ChunkLocator, bool, error) {
	return getLocatorTx(ctx, tx.conn(), "postgres tx", hash)
}

func (tx *postgresTransaction) DeleteSynced(ctx context.Context, hash block.ContentHash) error {
	return deleteSyncedTx(ctx, tx.conn(), "postgres tx", hash)
}

// ============================================================================
// Shared bodies
// ============================================================================
//
// Each takes the caller's op prefix ("postgres" or "postgres tx") so a failure
// still records whether it happened on the pool path or inside a transaction.
// Merging the bodies would otherwise erase that distinction, and it is the one
// worth keeping here: the failures these report are usually visibility or
// locking problems, where knowing there was an open transaction is the answer.

// isSyncedTx reports whether a synced marker exists for hash. An absent row is
// "not yet synced", not an error.
func isSyncedTx(ctx context.Context, x storesql.Executor, op string, hash block.ContentHash) (bool, error) {
	var dummy int
	err := x.QueryRow(ctx, `SELECT 1 FROM synced_hashes WHERE hash = $1`, hash[:]).Scan(&dummy)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s synced get: %w", op, err)
	}
	return true, nil
}

// markSyncedTx records that hash has been mirrored to remote, persisting loc's
// block columns atomically. Idempotent via ON CONFLICT (hash) DO NOTHING —
// re-applying the same hash is a no-op that preserves the first locator. A
// standalone locator (BlockID == "") leaves the block columns NULL, identical to
// a pre-locator row, so existing data needs no migration.
func markSyncedTx(ctx context.Context, x storesql.Executor, op string, hash block.ContentHash, loc block.ChunkLocator) error {
	blockID, off, length := locatorArgs(loc)
	if _, err := x.Exec(ctx,
		`INSERT INTO synced_hashes (hash, synced_at, block_id, block_offset, block_length)
			VALUES ($1, NOW(), $2, $3, $4)
			ON CONFLICT (hash) DO NOTHING`,
		hash[:], blockID, off, length); err != nil {
		return fmt.Errorf("%s synced mark: %w", op, err)
	}
	return nil
}

// getLocatorTx returns the recorded remote locator for hash: (zero, false, nil)
// when no row exists; a synced row with NULL/empty block columns yields the zero
// (standalone) locator with found == true.
func getLocatorTx(ctx context.Context, x storesql.Executor, op string, hash block.ContentHash) (block.ChunkLocator, bool, error) {
	var blockID sql.NullString
	var off, length sql.NullInt64
	err := x.QueryRow(ctx,
		`SELECT block_id, block_offset, block_length FROM synced_hashes WHERE hash = $1`,
		hash[:]).Scan(&blockID, &off, &length)
	if errors.Is(err, pgx.ErrNoRows) {
		return block.ChunkLocator{}, false, nil
	}
	if err != nil {
		return block.ChunkLocator{}, false, fmt.Errorf("%s synced get locator: %w", op, err)
	}
	if !blockID.Valid || blockID.String == "" {
		return block.ChunkLocator{}, true, nil
	}
	if !off.Valid || !length.Valid {
		return block.ChunkLocator{}, false, fmt.Errorf("corrupt locator row: block_id %q with NULL offset/length", blockID.String)
	}
	return block.ChunkLocator{BlockID: blockID.String, WireOffset: off.Int64, WireLength: length.Int64}, true, nil
}

// deleteSyncedTx removes the synced marker for hash. Idempotent: zero rows
// affected is not an error.
func deleteSyncedTx(ctx context.Context, x storesql.Executor, op string, hash block.ContentHash) error {
	if _, err := x.Exec(ctx, `DELETE FROM synced_hashes WHERE hash = $1`, hash[:]); err != nil {
		return fmt.Errorf("%s synced delete: %w", op, err)
	}
	return nil
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

func (tx *postgresTransaction) PutSyncedLocators(ctx context.Context, chunks []block.BlockChunkCommit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(chunks) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, c := range chunks {
		blockID, off, length := locatorArgs(c.Remote)
		batch.Queue(
			`INSERT INTO synced_hashes (hash, synced_at, block_id, block_offset, block_length)
				VALUES ($1, NOW(), $2, $3, $4)
				ON CONFLICT (hash) DO UPDATE SET
					synced_at = excluded.synced_at,
					block_id = excluded.block_id,
					block_offset = excluded.block_offset,
					block_length = excluded.block_length`,
			c.Hash[:], blockID, off, length)
	}
	br := tx.tx.SendBatch(ctx, batch)
	defer func() { _ = br.Close() }()
	for range chunks {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("postgres tx synced put locators: %w", err)
		}
	}
	return nil
}
