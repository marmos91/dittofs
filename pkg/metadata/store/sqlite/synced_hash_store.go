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

// MarkSynced records that hash has been mirrored to remote. Idempotent
// via ON CONFLICT (hash) DO NOTHING — re-applying the same hash is a
// no-op.
func (s *SQLiteMetadataStore) MarkSynced(ctx context.Context, hash block.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if _, err := s.exec(ctx,
		`INSERT INTO synced_hashes (hash, synced_at) VALUES (?1, CURRENT_TIMESTAMP)
			ON CONFLICT (hash) DO NOTHING`,
		hash[:]); err != nil {
		return fmt.Errorf("sqlite synced mark: %w", err)
	}
	return nil
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
