-- Phase 10 LSL-05: per-file rollup_offset persistence for the hybrid
-- append-log tier. Backs metadata.RollupStore for Postgres.
--
-- The application enforces INV-03 (atomic-monotone) via a conditional
-- WHERE predicate on the ON CONFLICT DO UPDATE branch:
--
--   INSERT INTO rollup_offsets (payload_id, rollup_offset) VALUES ($1, $2)
--   ON CONFLICT (payload_id) DO UPDATE
--       SET rollup_offset = EXCLUDED.rollup_offset
--       WHERE rollup_offsets.rollup_offset <= EXCLUDED.rollup_offset;
--
-- A rejected regression produces RowsAffected()=0 — the app surfaces that
-- as metadata.ErrRollupOffsetRegression with the stored value unchanged.
--
-- Schema is minimal by design: payload_id is the primary key (one row per
-- file), rollup_offset holds the byte offset of the first un-rolled-up
-- record in the per-file append log. Future phases may fold this into a
-- broader per-file metadata row (see 10-CONTEXT.md D-12 atomicity contract
-- and the A3 per-file row rework).

CREATE TABLE IF NOT EXISTS rollup_offsets (
    payload_id    TEXT PRIMARY KEY,
    rollup_offset BIGINT NOT NULL,

    CONSTRAINT valid_rollup_offset CHECK (rollup_offset >= 0)
);
