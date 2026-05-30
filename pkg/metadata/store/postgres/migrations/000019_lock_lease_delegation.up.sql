-- Migration: persist SMB lease and NFSv4 delegation state on locks.
--
-- The locks table previously stored only the byte-range subset of
-- PersistedLock, silently dropping every lease and delegation field on
-- PutLock. A restored lease lost its R/W/H caching state, V2 parent key,
-- in-flight break target, directory flag, and traditional-oplock tier; a
-- restored delegation lost its id, type, and recall/revoke state entirely.
-- These columns close that gap so the Postgres backend round-trips the full
-- PersistedLock like memory and badger do.
--
-- All columns are nullable / zero-defaulted so existing byte-range rows
-- (which carry none of this state) migrate without rewrite.

-- Lease fields.
ALTER TABLE locks ADD COLUMN IF NOT EXISTS lease_key BYTEA;
ALTER TABLE locks ADD COLUMN IF NOT EXISTS lease_state BIGINT NOT NULL DEFAULT 0;
ALTER TABLE locks ADD COLUMN IF NOT EXISTS lease_epoch INTEGER NOT NULL DEFAULT 0;
ALTER TABLE locks ADD COLUMN IF NOT EXISTS break_to_state BIGINT NOT NULL DEFAULT 0;
ALTER TABLE locks ADD COLUMN IF NOT EXISTS breaking_to_required BIGINT NOT NULL DEFAULT 0;
ALTER TABLE locks ADD COLUMN IF NOT EXISTS breaking BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE locks ADD COLUMN IF NOT EXISTS parent_lease_key BYTEA;
ALTER TABLE locks ADD COLUMN IF NOT EXISTS is_directory BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE locks ADD COLUMN IF NOT EXISTS is_traditional_oplock BOOLEAN NOT NULL DEFAULT FALSE;

-- Delegation fields.
ALTER TABLE locks ADD COLUMN IF NOT EXISTS delegation_id TEXT NOT NULL DEFAULT '';
ALTER TABLE locks ADD COLUMN IF NOT EXISTS deleg_type SMALLINT NOT NULL DEFAULT 0;
ALTER TABLE locks ADD COLUMN IF NOT EXISTS deleg_breaking BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE locks ADD COLUMN IF NOT EXISTS deleg_recalled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE locks ADD COLUMN IF NOT EXISTS deleg_revoked BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE locks ADD COLUMN IF NOT EXISTS deleg_notification_mask BIGINT NOT NULL DEFAULT 0;
