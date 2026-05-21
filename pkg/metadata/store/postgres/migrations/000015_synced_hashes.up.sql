-- Per-CAS-hash synced marker table backing metadata.SyncedHashStore.
-- Presence-of-row means the chunk has been mirrored to the remote store
-- at least once; idempotent INSERT via ON CONFLICT DO NOTHING; idempotent
-- DELETE on refcount-cascade. Hash is the raw 32-byte BLAKE3-256 digest;
-- synced_at preserved for future observability.

CREATE TABLE IF NOT EXISTS synced_hashes (
    hash       BYTEA PRIMARY KEY,
    synced_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT valid_hash_length CHECK (octet_length(hash) = 32)
);
