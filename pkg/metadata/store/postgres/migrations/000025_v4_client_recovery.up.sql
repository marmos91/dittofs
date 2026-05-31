-- Migration: NFSv4 client-recovery records for reboot/grace recovery (area-4 H8).
--
-- Persists the set of confirmed NFSv4 client identities so that after a server
-- restart the grace machinery knows which clients may reclaim their prior state
-- (RFC 7530 §9.1.4 / RFC 8881 §8.4.2). This is the DittoFS analog of the Linux
-- nfsd client-record: id + boot verifier + principal only — no opens, no locks,
-- no stateids.
--
-- Records are server-GLOBAL (the v4 clientid namespace is not owned by any
-- share), keyed by clientid_string (nfs_client_id4.id, the stable client
-- identity). reclaim_complete records that a client finished reclaim so a
-- SECOND restart inside one grace window does not wait on an already-done
-- client; it defaults FALSE.
--
-- This slice adds only the table; the v4 state-manager wiring lands separately.

CREATE TABLE IF NOT EXISTS v4_client_recovery (
    clientid_string  TEXT PRIMARY KEY,
    clientid         BIGINT NOT NULL,
    boot_verifier    BYTEA NOT NULL,
    principal        TEXT NOT NULL,
    confirmed_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    server_epoch     BIGINT NOT NULL,
    reclaim_complete BOOLEAN NOT NULL DEFAULT FALSE,

    CONSTRAINT valid_boot_verifier_length CHECK (length(boot_verifier) = 8)
);

-- Index on server_epoch for stale-record GC across server instances.
CREATE INDEX IF NOT EXISTS idx_v4_client_recovery_server_epoch ON v4_client_recovery(server_epoch);
