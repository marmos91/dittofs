// Per-CAS-hash idempotent presence marker backed by the synced_hashes
// table. See metadata.SyncedHashStore for the contract; this backend
// uses a tiny indexed table keyed by the raw 32-byte BLAKE3 hash, with
// ON CONFLICT DO NOTHING for idempotent MarkSynced and unconditional
// DELETE for idempotent DeleteSynced.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Compile-time assertion: the Postgres engine implements SyncedHashStore.
var _ metadata.SyncedHashStore = (*PostgresMetadataStore)(nil)

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

// MarkSynced records that hash has been mirrored to remote. Idempotent
// via ON CONFLICT (hash) DO NOTHING — re-applying the same hash is a
// no-op.
func (s *PostgresMetadataStore) MarkSynced(ctx context.Context, hash block.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if _, err := s.exec(ctx,
		`INSERT INTO synced_hashes (hash, synced_at) VALUES ($1, NOW())
			ON CONFLICT (hash) DO NOTHING`,
		hash[:]); err != nil {
		return fmt.Errorf("postgres synced mark: %w", err)
	}
	return nil
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
