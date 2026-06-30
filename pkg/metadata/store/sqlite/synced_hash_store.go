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

// Compile-time assertion: the SQLite engine implements SyncedHashStore.
var _ metadata.SyncedHashStore = (*SQLiteMetadataStore)(nil)

// EnumerateSynced streams every synced marker with its first-mirror time,
// read straight from the synced_hashes table. Used by the LIST-free GC sweep
// to compute remote-orphan candidates without an S3 LIST.
func (s *SQLiteMetadataStore) EnumerateSynced(ctx context.Context, fn func(hash block.ContentHash, syncedAt time.Time) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rows, err := s.query(ctx, `SELECT hash, synced_at FROM synced_hashes`)
	if err != nil {
		return fmt.Errorf("sqlite synced enumerate: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			raw      []byte
			syncedAt time.Time
		)
		if err := rows.Scan(&raw, &syncedAt); err != nil {
			return fmt.Errorf("sqlite synced enumerate scan: %w", err)
		}
		if len(raw) != len(block.ContentHash{}) {
			// Defensive: a malformed hash row cannot be reduced to a
			// ContentHash. Skip it rather than corrupt the sweep candidate.
			continue
		}
		var h block.ContentHash
		copy(h[:], raw)
		if err := fn(h, syncedAt); err != nil {
			return err
		}
	}
	return rows.Err()
}

// IsSynced reports whether hash has been MarkSynced'd at least once.
// Returns (false, nil) when no row exists for hash — an absent hash is
// treated as "not yet synced", not as an error.
func (s *SQLiteMetadataStore) IsSynced(ctx context.Context, hash block.ContentHash) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	row := s.queryRow(ctx,
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

// MarkSynced records that hash has been mirrored to remote, persisting loc's
// block columns atomically. Idempotent via ON CONFLICT (hash) DO NOTHING —
// re-applying the same hash is a no-op that preserves the first locator. A
// standalone locator (BlockID == "") leaves the block columns NULL, identical to
// a pre-locator row, so existing data needs no migration.
func (s *SQLiteMetadataStore) MarkSynced(ctx context.Context, hash block.ContentHash, loc block.ChunkLocator) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	blockID, off, length := locatorArgs(loc)
	if _, err := s.exec(ctx,
		`INSERT INTO synced_hashes (hash, synced_at, block_id, block_offset, block_length)
			VALUES (?1, CURRENT_TIMESTAMP, ?2, ?3, ?4)
			ON CONFLICT (hash) DO NOTHING`,
		hash[:], blockID, off, length); err != nil {
		return fmt.Errorf("sqlite synced mark: %w", err)
	}
	return nil
}

// GetLocator returns the recorded remote locator for hash. (zero, false, nil)
// when no row exists; a synced row with NULL/empty block columns yields the zero
// (standalone) locator with found == true.
func (s *SQLiteMetadataStore) GetLocator(ctx context.Context, hash block.ContentHash) (block.ChunkLocator, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.ChunkLocator{}, false, err
	}

	row := s.queryRow(ctx,
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

// locatorArgs maps a ChunkLocator onto the (block_id, block_offset, block_length)
// INSERT args: NULL for a standalone chunk (so its row is identical to a
// pre-locator row), the recorded values for a block-resident chunk.
func locatorArgs(loc block.ChunkLocator) (blockID, off, length any) {
	if loc.IsStandalone() {
		return nil, nil, nil
	}
	return loc.BlockID, loc.Offset, loc.Length
}

// scanLocatorRow scans a (block_id, block_offset, block_length) row into a
// ChunkLocator. NULL/empty block_id yields the zero (standalone) locator.
func scanLocatorRow(row scanRow) (block.ChunkLocator, error) {
	var blockID sql.NullString
	var off, length sql.NullInt64
	if err := row.Scan(&blockID, &off, &length); err != nil {
		return block.ChunkLocator{}, err
	}
	if !blockID.Valid || blockID.String == "" {
		return block.ChunkLocator{}, nil
	}
	return block.ChunkLocator{BlockID: blockID.String, Offset: off.Int64, Length: length.Int64}, nil
}

// DeleteSynced removes the synced marker for hash. Idempotent: DELETE
// returns no error when the row does not exist (zero rows affected is
// not an error condition).
func (s *SQLiteMetadataStore) DeleteSynced(ctx context.Context, hash block.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if _, err := s.exec(ctx,
		`DELETE FROM synced_hashes WHERE hash = ?1`,
		hash[:]); err != nil {
		return fmt.Errorf("sqlite synced delete: %w", err)
	}
	return nil
}
