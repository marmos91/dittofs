package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// RollupStore Implementation for PostgreSQL Store
// ============================================================================
//
// Persists the per-file rollup_offset for the hybrid append-log tier. The
// atomic-monotone contract is enforced by reading the prior value under a
// FOR UPDATE row lock and rejecting a strictly-lower offset before the
// upsert commits — the lock closes the read-modify-write race window, so a
// concurrent writer cannot interleave between the check and the write.
//
// Schema lives in migration 000009 (rollup_offsets table). The migration
// is idempotent (`CREATE TABLE IF NOT EXISTS`), so a re-run on an
// already-migrated database is a cheap no-op.
// ============================================================================

// Compile-time assertion: the Postgres engine implements RollupStore.
var _ metadata.RollupStore = (*PostgresMetadataStore)(nil)

// validateStoredOffset converts a BIGINT-decoded rollup offset to uint64,
// rejecting negative values that can only arise from on-disk corruption
// or out-of-band SQL writes (FIX-14: the write path bounds-checks against
// MaxInt64 and uint64 inputs cannot produce a negative cast).
func validateStoredOffset(v int64) (uint64, error) {
	if v < 0 {
		return 0, fmt.Errorf("postgres rollup: stored offset %d is negative (corruption)", v)
	}
	return uint64(v), nil
}

// SetRollupOffset atomically advances payloadID -> newOffset iff
// newOffset >= the currently-stored value. Returns the PREVIOUS stored
// value on success.
//
// On monotone violation (newOffset < stored), returns (storedOffset,
// metadata.ErrRollupOffsetRegression); the stored value is UNCHANGED.
//
// Atomicity: the monotone invariant (INV-03) is enforced by the conditional
// WHERE predicate on the ON CONFLICT DO UPDATE branch — a regression performs
// no write and reports RowsAffected()==0. This holds even for concurrent
// first-inserts (no row to FOR UPDATE-lock): they converge under the
// unique-key conflict and the WHERE guard rejects the lower offset.
//
// The prior value is read separately (FOR UPDATE) only to return `prev` on
// success. It is NOT projected out of an
// INSERT ... ON CONFLICT ... RETURNING (SELECT rollup_offset FROM cte): the
// CTE and the conflict target are the same row, so that subquery resolves to
// NULL and reported prev=0 on every update.
func (s *PostgresMetadataStore) SetRollupOffset(ctx context.Context, payloadID string, newOffset uint64) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	// FIX-14: Postgres rollup_offset is stored as BIGINT (signed int64). Reject
	// any newOffset that does not fit so we never overflow the column with a
	// silently-truncated cast. In practice no real file approaches 2^63 bytes,
	// but the guard prevents a cast-induced negative-offset corruption from
	// reaching the database.
	if newOffset > math.MaxInt64 {
		return 0, fmt.Errorf("postgres: rollup offset %d exceeds BIGINT range", newOffset)
	}

	tx, err := s.beginTx(ctx)
	if err != nil {
		return 0, fmt.Errorf("postgres rollup begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock + read the prior value for the returned `prev`. ErrNoRows means no
	// row yet (prev == 0); FOR UPDATE then locks nothing, so the monotone
	// invariant for that case CANNOT rely on this read — two concurrent
	// first-inserts both see no row. The conditional WHERE predicate on the
	// upsert below is the authority (see migration 000009 / INV-03).
	var prev uint64
	var prevSigned int64
	switch err := tx.QueryRow(ctx,
		`SELECT rollup_offset FROM rollup_offsets WHERE payload_id = $1 FOR UPDATE`,
		payloadID).Scan(&prevSigned); {
	case errors.Is(err, pgx.ErrNoRows):
		// No prior row yet: prev stays 0.
	case err != nil:
		return 0, fmt.Errorf("postgres rollup select-for-update: %w", err)
	default:
		v, verr := validateStoredOffset(prevSigned)
		if verr != nil {
			return 0, verr
		}
		prev = v
	}

	// Monotone upsert: the WHERE predicate rejects a strictly-lower offset on
	// the conflict branch, so a regression performs no write and reports
	// RowsAffected()==0. This is the single point that enforces INV-03 and is
	// safe even when no row existed at the SELECT above (concurrent
	// first-inserts converge here under the unique-key conflict).
	tag, err := tx.Exec(ctx,
		`INSERT INTO rollup_offsets (payload_id, rollup_offset)
		 VALUES ($1, $2)
		 ON CONFLICT (payload_id) DO UPDATE SET rollup_offset = EXCLUDED.rollup_offset
		 WHERE rollup_offsets.rollup_offset <= EXCLUDED.rollup_offset`,
		payloadID, int64(newOffset))
	if err != nil {
		return 0, fmt.Errorf("postgres rollup upsert: %w", err)
	}

	if tag.RowsAffected() == 0 {
		// Regression: a row exists with a strictly-higher offset (this caller's
		// newOffset < stored, possibly written by a racing first-insert). Read
		// the committed value to report; the stored value is left UNCHANGED
		// (deferred Rollback releases the lock without writing).
		var stored int64
		if err := tx.QueryRow(ctx,
			`SELECT rollup_offset FROM rollup_offsets WHERE payload_id = $1`,
			payloadID).Scan(&stored); err != nil {
			return 0, fmt.Errorf("postgres rollup read after regression: %w", err)
		}
		v, verr := validateStoredOffset(stored)
		if verr != nil {
			return 0, verr
		}
		return v, metadata.ErrRollupOffsetRegression
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("postgres rollup commit: %w", err)
	}
	return prev, nil
}

// GetRollupOffset returns the persisted rollup_offset for payloadID, or
// (0, nil) if unset. Matches the contract in metadata.RollupStore — a
// fresh file is treated as rolled-up-to-0.
func (s *PostgresMetadataStore) GetRollupOffset(ctx context.Context, payloadID string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	row := s.queryRow(ctx,
		`SELECT rollup_offset FROM rollup_offsets WHERE payload_id = $1`,
		payloadID)
	var v int64
	if err := row.Scan(&v); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("postgres rollup select: %w", err)
	}
	return validateStoredOffset(v)
}
