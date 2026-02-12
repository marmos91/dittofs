-- Migration: Add locks table for NLM/SMB lock persistence
-- This enables lock state to survive server restarts

CREATE TABLE IF NOT EXISTS locks (
    id              TEXT PRIMARY KEY,
    share_name      TEXT NOT NULL,
    file_id         TEXT NOT NULL,
    owner_id        TEXT NOT NULL,
    client_id       TEXT NOT NULL,
    lock_type       SMALLINT NOT NULL,
    byte_offset     BIGINT NOT NULL,
    byte_length     BIGINT NOT NULL,
    share_reservation SMALLINT NOT NULL DEFAULT 0,
    acquired_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    server_epoch    BIGINT NOT NULL,

    CONSTRAINT valid_lock_type CHECK (lock_type IN (0, 1)),
    CONSTRAINT valid_offset CHECK (byte_offset >= 0),
    CONSTRAINT valid_length CHECK (byte_length >= 0),
    CONSTRAINT valid_share_reservation CHECK (share_reservation >= 0 AND share_reservation <= 3)
);

-- Indexes for efficient queries
CREATE INDEX IF NOT EXISTS idx_locks_file_id ON locks(file_id);
CREATE INDEX IF NOT EXISTS idx_locks_owner_id ON locks(owner_id);
CREATE INDEX IF NOT EXISTS idx_locks_client_id ON locks(client_id);
CREATE INDEX IF NOT EXISTS idx_locks_share_name ON locks(share_name);

-- Server epoch tracking for split-brain detection
CREATE TABLE IF NOT EXISTS server_epoch (
    id          INTEGER PRIMARY KEY DEFAULT 1,
    epoch       BIGINT NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT single_row CHECK (id = 1)
);

-- Initialize epoch if table is empty
INSERT INTO server_epoch (id, epoch) VALUES (1, 0)
ON CONFLICT (id) DO NOTHING;
