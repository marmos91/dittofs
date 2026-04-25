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
// RollupStore Implementation for PostgreSQL Store (Phase 10 LSL-05)
// ============================================================================
//
// Persists the per-file rollup_offset for the hybrid append-log tier. The
// atomic-monotone contract (INV-03) is enforced at the DATABASE layer via
// a conditional WHERE predicate on the ON CONFLICT DO UPDATE branch — no
// read-modify-write race window exists application-side, because the
// engine itself rejects regressions.
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
// Atomicity: a single CTE-wrapped statement captures the prior row under
// FOR UPDATE (row lock) BEFORE the conditional upsert fires. The
// row-level lock excludes any concurrent writer for the duration of the
// statement, so the returned `prev` always reflects the value the upsert
// observed — never a stale snapshot from a parallel transaction.
//
// The conflict branch's WHERE predicate
// (rollup_offsets.rollup_offset <= EXCLUDED.rollup_offset) enforces the
// monotone invariant: if it rejects, neither INSERT nor UPDATE fires and
// the RETURNING clause yields no rows — we surface that as the
// regression sentinel and return the locked prior value.
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

	// Single-statement atomic upsert: the `prev` CTE locks the existing
	// row (if any) with FOR UPDATE, then the INSERT ... ON CONFLICT runs
	// in the same statement under that lock. RETURNING yields the prior
	// value (or NULL on first insert) alongside the new value when the
	// monotone guard accepts the write.
	//
	// On regression (WHERE predicate false on the conflict branch),
	// neither INSERT nor UPDATE fires, RETURNING yields zero rows, and
	// the row-lock release exposes the unchanged prior value to the
	// follow-up read below.
	const upsertSQL = `
		WITH prev AS (
			SELECT rollup_offset FROM rollup_offsets
			WHERE payload_id = $1
			FOR UPDATE
		)
		INSERT INTO rollup_offsets (payload_id, rollup_offset)
		VALUES ($1, $2)
		ON CONFLICT (payload_id) DO UPDATE
			SET rollup_offset = EXCLUDED.rollup_offset
			WHERE rollup_offsets.rollup_offset <= EXCLUDED.rollup_offset
		RETURNING (SELECT rollup_offset FROM prev), rollup_offset
	`

	var (
		prevSigned    *int64 // nullable: NULL on first insert
		currentSigned int64
	)
	row := s.queryRow(ctx, upsertSQL, payloadID, int64(newOffset))
	switch err := row.Scan(&prevSigned, &currentSigned); {
	case errors.Is(err, pgx.ErrNoRows):
		// Regression: monotone guard rejected the update. Read the
		// locked-and-released prior value to surface in the sentinel
		// error. The lock from the CTE has been released by the time
		// this query runs, but a concurrent writer at this point would
		// only ever advance the value monotonically — the regression
		// sentinel remains correct (newOffset < stored holds).
		var stored int64
		row2 := s.queryRow(ctx,
			`SELECT rollup_offset FROM rollup_offsets WHERE payload_id = $1`,
			payloadID)
		if err2 := row2.Scan(&stored); err2 != nil {
			return 0, fmt.Errorf("postgres rollup read after regression: %w", err2)
		}
		v, verr := validateStoredOffset(stored)
		if verr != nil {
			return 0, verr
		}
		return v, metadata.ErrRollupOffsetRegression
	case err != nil:
		return 0, fmt.Errorf("postgres rollup upsert: %w", err)
	}

	if prevSigned == nil {
		return 0, nil
	}
	return validateStoredOffset(*prevSigned)
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
