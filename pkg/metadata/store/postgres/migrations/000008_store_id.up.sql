-- Phase 5 D-06: engine-persistent store identifier (Pitfall #4 mitigation).
--
-- Adds a stable store_id column to server_config. Used by Phase 5 restore
-- to gate cross-store contamination: the manifest's store_id is compared
-- against the target engine's persistent store_id, and any mismatch is
-- rejected with ErrStoreIDMismatch.
--
-- The column defaults to '' (empty string). Application-layer bootstrap
-- (PostgresMetadataStore.ensureStoreID) writes a fresh ULID on first open
-- if the row carries the empty sentinel. Subsequent opens read the
-- existing value.

ALTER TABLE server_config
    ADD COLUMN IF NOT EXISTS store_id VARCHAR(36) NOT NULL DEFAULT '';

-- Backfill any pre-existing NULL (defensive; the NOT NULL above should
-- prevent this, but some older PostgreSQL dialects leave NULLs on
-- ADD COLUMN when a DEFAULT is present).
UPDATE server_config SET store_id = '' WHERE store_id IS NULL;
