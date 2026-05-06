-- Phase 14 Plan 01 (MIG-03 / D-A6): per-share block_layout flag.
--
-- Adds a dedicated text column to the shares table that tracks which
-- block-key scheme each share is currently using:
--
--   * 'legacy'   — keys under the v0.13/v0.14 path-indexed scheme
--                  `{payloadID}/block-{idx}`. The dual-read shim is
--                  active.
--   * 'cas-only' — keys under the v0.15 CAS scheme
--                  `cas/{hh}/{hh}/{hash_hex}`. No shim.
--
-- DEFAULT 'legacy' is critical: rows that pre-date this migration
-- (every v0.13/v0.14 share) MUST be readable as legacy until the
-- operator runs `dfsctl blockstore migrate`, which flips the flag to
-- cas-only inside the same metadata transaction that deletes the
-- legacy keys.
--
-- NOT NULL prevents the empty-string sentinel from sneaking in via
-- INSERT; ParseBlockLayout still tolerates "" on read for any
-- backend that produces it (e.g. memory zero-value), but the DB
-- itself enforces the invariant.

ALTER TABLE shares
    ADD COLUMN IF NOT EXISTS block_layout TEXT NOT NULL DEFAULT 'legacy';
