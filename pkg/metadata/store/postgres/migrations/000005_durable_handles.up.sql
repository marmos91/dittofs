-- Durable handle state for SMB3 durable opens (Phase 38)
-- Stores the full OpenFile state needed for reconnection after client disconnect.
-- Supports multi-key lookups for reconnect validation, conflict checking, and failover.

CREATE TABLE durable_handles (
    id                TEXT PRIMARY KEY,
    file_id           BYTEA NOT NULL,
    path              TEXT NOT NULL,
    share_name        TEXT NOT NULL,
    desired_access    INTEGER NOT NULL,
    share_access      INTEGER NOT NULL,
    create_options    INTEGER NOT NULL,
    metadata_handle   BYTEA NOT NULL,
    payload_id        TEXT,
    oplock_level      SMALLINT NOT NULL DEFAULT 0,
    lease_key         BYTEA,
    lease_state       INTEGER NOT NULL DEFAULT 0,
    create_guid       BYTEA,
    app_instance_id   BYTEA,
    username          TEXT NOT NULL,
    session_key_hash  BYTEA NOT NULL,
    is_v2             BOOLEAN NOT NULL DEFAULT FALSE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    disconnected_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    timeout_ms        INTEGER NOT NULL DEFAULT 60000,
    server_start_time TIMESTAMPTZ NOT NULL,

    CONSTRAINT valid_file_id CHECK (length(file_id) = 16),
    CONSTRAINT valid_session_key_hash CHECK (length(session_key_hash) = 32)
);

-- Index for V2 reconnect: lookup by CreateGuid
CREATE INDEX idx_durable_handles_create_guid ON durable_handles(create_guid) WHERE create_guid IS NOT NULL;

-- Index for Hyper-V failover: lookup by AppInstanceId
CREATE INDEX idx_durable_handles_app_instance_id ON durable_handles(app_instance_id) WHERE app_instance_id IS NOT NULL;

-- Index for V1 reconnect: lookup by FileID
CREATE INDEX idx_durable_handles_file_id ON durable_handles(file_id);

-- Index for share-level management
CREATE INDEX idx_durable_handles_share_name ON durable_handles(share_name);

-- Index for conflict checking: lookup by metadata file handle
CREATE INDEX idx_durable_handles_metadata_handle ON durable_handles(metadata_handle);

-- Index for scavenger: expired handle cleanup
CREATE INDEX idx_durable_handles_disconnected_at ON durable_handles(disconnected_at);
