-- Migration: Add nsm_client_registrations table for NSM crash recovery
-- This enables NSM client registrations to survive server restarts

CREATE TABLE IF NOT EXISTS nsm_client_registrations (
    client_id       TEXT PRIMARY KEY,
    mon_name        TEXT NOT NULL,
    priv            BYTEA NOT NULL,
    callback_host   TEXT NOT NULL,
    callback_prog   INTEGER NOT NULL,
    callback_vers   INTEGER NOT NULL,
    callback_proc   INTEGER NOT NULL,
    registered_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    server_epoch    BIGINT NOT NULL,

    CONSTRAINT valid_priv_length CHECK (length(priv) = 16),
    CONSTRAINT valid_callback_prog CHECK (callback_prog >= 0),
    CONSTRAINT valid_callback_vers CHECK (callback_vers >= 0),
    CONSTRAINT valid_callback_proc CHECK (callback_proc >= 0)
);

-- Indexes for efficient queries
-- Index on callback_host for SM_UNMON_ALL queries
CREATE INDEX IF NOT EXISTS idx_nsm_client_registrations_callback_host ON nsm_client_registrations(callback_host);
-- Index on registered_at for ordering/cleanup
CREATE INDEX IF NOT EXISTS idx_nsm_client_registrations_registered_at ON nsm_client_registrations(registered_at);
-- Index on mon_name for DeleteClientRegistrationsByMonName
CREATE INDEX IF NOT EXISTS idx_nsm_client_registrations_mon_name ON nsm_client_registrations(mon_name);
