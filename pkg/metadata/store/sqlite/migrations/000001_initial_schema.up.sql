-- SQLite metadata store schema.
--
-- This single consolidated migration produces the SAME logical schema the
-- Postgres backend reaches after its 34 incremental migrations. SQLite has no
-- existing deployments to migrate incrementally, so the final state is created
-- in one step. Dialect adaptations vs Postgres:
--   UUID      -> TEXT  (UUID stored as its canonical string form)
--   BYTEA     -> BLOB
--   JSONB     -> TEXT  (JSON stored as text/bytes)
--   BOOLEAN   -> INTEGER 0/1 (the database/sql driver maps Go bool transparently)
--   TIMESTAMPTZ NOW() -> TEXT CURRENT_TIMESTAMP for server-managed audit columns;
--                        file timestamps remain BIGINT unix-nanoseconds (lossless).
--   NUMERIC(20) lock offset/length -> INTEGER (uint64 fits SQLite's 64-bit int;
--                        values are stored/read as int64 by the lock codec).
-- ON DELETE CASCADE is honoured because every connection enables
-- PRAGMA foreign_keys=ON (see config.DSN).

-- ---------------------------------------------------------------------------
-- inodes (Postgres "files", renamed in migration 000032; path/path_hash dropped
-- in #1166 — namespace lives entirely in parent_child_map).
-- ---------------------------------------------------------------------------
CREATE TABLE inodes (
    id              TEXT PRIMARY KEY,
    share_name      TEXT NOT NULL,
    file_type       INTEGER NOT NULL,
    mode            INTEGER NOT NULL,
    uid             INTEGER NOT NULL,
    gid             INTEGER NOT NULL,
    size            INTEGER NOT NULL DEFAULT 0,
    nlink           INTEGER NOT NULL DEFAULT 1,
    atime           INTEGER NOT NULL,
    mtime           INTEGER NOT NULL,
    ctime           INTEGER NOT NULL,
    creation_time   INTEGER NOT NULL,
    content_id      TEXT,
    link_target     TEXT,
    device_major    INTEGER,
    device_minor    INTEGER,
    hidden          INTEGER NOT NULL DEFAULT 0,
    acl             TEXT,
    eas             TEXT,
    object_id       BLOB,
    deleted_at      INTEGER,
    original_path   TEXT NOT NULL DEFAULT '',
    deleted_by      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,

    CONSTRAINT valid_file_type CHECK (file_type BETWEEN 0 AND 6),
    CONSTRAINT valid_uid CHECK (uid >= 0),
    CONSTRAINT valid_gid CHECK (gid >= 0),
    CONSTRAINT valid_size CHECK (size >= 0),
    CONSTRAINT valid_nlink CHECK (nlink >= 0)
);

CREATE INDEX idx_inodes_share_name ON inodes(share_name);
-- SQLite has no btree row-size limit, so content_id is indexed directly
-- (the Postgres backend hashed it into content_id_hash to dodge the 2704-byte
-- limit; that workaround is unnecessary here).
CREATE INDEX idx_inodes_content_id ON inodes(content_id) WHERE content_id IS NOT NULL;
CREATE INDEX idx_inodes_updated_at ON inodes(updated_at);
CREATE INDEX idx_inodes_hidden ON inodes(hidden) WHERE hidden = 1;
CREATE UNIQUE INDEX inodes_object_id_idx ON inodes(object_id) WHERE object_id IS NOT NULL;
CREATE INDEX idx_inodes_uid ON inodes(uid) WHERE file_type = 0;
CREATE INDEX idx_inodes_gid ON inodes(gid) WHERE file_type = 0;

-- ---------------------------------------------------------------------------
-- parent_child_map: the namespace. No unique constraint on child_id, so an
-- inode may be referenced by many names (hard links).
-- ---------------------------------------------------------------------------
CREATE TABLE parent_child_map (
    parent_id   TEXT NOT NULL REFERENCES inodes(id) ON DELETE CASCADE,
    child_id    TEXT NOT NULL REFERENCES inodes(id) ON DELETE CASCADE,
    child_name  TEXT NOT NULL,

    PRIMARY KEY (parent_id, child_name),
    CONSTRAINT non_empty_child_name CHECK (length(child_name) > 0)
);

CREATE INDEX idx_parent_child_map_parent ON parent_child_map(parent_id);
CREATE INDEX idx_parent_child_map_child ON parent_child_map(child_id);

-- ---------------------------------------------------------------------------
-- shares
-- ---------------------------------------------------------------------------
CREATE TABLE shares (
    share_name      TEXT PRIMARY KEY,
    root_file_id    TEXT NOT NULL REFERENCES inodes(id),
    options         TEXT NOT NULL DEFAULT '{}',
    block_layout    TEXT NOT NULL DEFAULT 'legacy',
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,

    CONSTRAINT non_empty_share_name CHECK (length(share_name) > 0)
);

-- ---------------------------------------------------------------------------
-- filesystem_meta
-- ---------------------------------------------------------------------------
CREATE TABLE filesystem_meta (
    share_name TEXT PRIMARY KEY,
    meta       TEXT NOT NULL DEFAULT '{}'
);

-- ---------------------------------------------------------------------------
-- filesystem_capabilities (singleton)
-- ---------------------------------------------------------------------------
CREATE TABLE filesystem_capabilities (
    id                   INTEGER PRIMARY KEY DEFAULT 1,
    max_read_size        INTEGER NOT NULL,
    preferred_read_size  INTEGER NOT NULL,
    max_write_size       INTEGER NOT NULL,
    preferred_write_size INTEGER NOT NULL,
    max_file_size        INTEGER NOT NULL,
    max_filename_len     INTEGER NOT NULL,
    max_path_len         INTEGER NOT NULL,
    max_hard_link_count  INTEGER NOT NULL,
    supports_hard_links  INTEGER NOT NULL,
    supports_symlinks    INTEGER NOT NULL,
    case_sensitive       INTEGER NOT NULL,
    case_preserving      INTEGER NOT NULL,
    supports_acls        INTEGER NOT NULL,
    time_resolution      INTEGER NOT NULL,

    CONSTRAINT singleton_check CHECK (id = 1)
);

-- ---------------------------------------------------------------------------
-- server_config (singleton): config JSON and the engine-persistent store_id.
-- ---------------------------------------------------------------------------
CREATE TABLE server_config (
    id         INTEGER PRIMARY KEY DEFAULT 1,
    config     TEXT NOT NULL DEFAULT '{}',
    store_id   TEXT,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,

    CONSTRAINT singleton_check CHECK (id = 1)
);

-- ---------------------------------------------------------------------------
-- server_epoch (singleton): NLM/SMB lock-generation counter PLUS the
-- clean-shutdown marker. The row is created lazily by IncrementServerEpoch /
-- SetCleanShutdown; an absent row reads as epoch 0 / unclean (the fail-safe
-- default the boot-time lock recovery relies on after a Reset).
-- ---------------------------------------------------------------------------
CREATE TABLE server_epoch (
    id             INTEGER PRIMARY KEY DEFAULT 1,
    epoch          INTEGER NOT NULL DEFAULT 0,
    clean_shutdown INTEGER NOT NULL DEFAULT 0,
    updated_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,

    CONSTRAINT singleton_check CHECK (id = 1)
);

-- ---------------------------------------------------------------------------
-- file_blocks: content-addressed block metadata.
-- ---------------------------------------------------------------------------
CREATE TABLE file_blocks (
    id                   TEXT PRIMARY KEY,
    hash                 TEXT,
    data_size            INTEGER NOT NULL DEFAULT 0,
    cache_path           TEXT,
    block_store_key      TEXT,
    ref_count            INTEGER NOT NULL DEFAULT 1,
    last_access          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_at           TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    state                INTEGER NOT NULL DEFAULT 0,
    last_sync_attempt_at TIMESTAMP
);

CREATE INDEX idx_file_blocks_hash ON file_blocks(hash) WHERE hash IS NOT NULL;
CREATE INDEX idx_file_blocks_pending ON file_blocks(created_at) WHERE state = 0 AND cache_path IS NOT NULL;
CREATE INDEX idx_file_blocks_remote ON file_blocks(last_access) WHERE state = 2 AND cache_path IS NOT NULL;
CREATE INDEX idx_file_blocks_unreferenced ON file_blocks(id) WHERE ref_count = 0;
CREATE INDEX idx_file_blocks_syncing_age ON file_blocks(last_sync_attempt_at) WHERE state = 1;

-- ---------------------------------------------------------------------------
-- file_block_refs: per-file ordered block manifest.
-- ---------------------------------------------------------------------------
CREATE TABLE file_block_refs (
    file_id  TEXT NOT NULL REFERENCES inodes(id) ON DELETE CASCADE,
    "offset" INTEGER NOT NULL,
    size     INTEGER NOT NULL,
    hash     BLOB NOT NULL,

    PRIMARY KEY (file_id, "offset")
);

CREATE INDEX idx_file_block_refs_file_id ON file_block_refs(file_id);

-- ---------------------------------------------------------------------------
-- locks: persisted NLM/SMB lock state.
-- ---------------------------------------------------------------------------
CREATE TABLE locks (
    id                      TEXT PRIMARY KEY,
    share_name              TEXT NOT NULL,
    -- file_id is an opaque lock target (NFS/SMB file handle), NOT necessarily an
    -- inodes.id, so it carries no foreign key (matching the Postgres schema).
    file_id                 TEXT NOT NULL,
    owner_id                TEXT NOT NULL DEFAULT '',
    client_id               TEXT NOT NULL,
    lock_type               INTEGER NOT NULL,
    -- byte_offset/byte_length hold uint64 values up to 0xFFFFFFFFFFFFFFFF,
    -- which exceeds SQLite's signed 64-bit INTEGER. The lock codec stores them
    -- as decimal strings (strconv.FormatUint) and parses them back, so TEXT
    -- affinity preserves the full unsigned range losslessly.
    byte_offset             TEXT,
    byte_length             TEXT,
    is_zero_byte            INTEGER NOT NULL DEFAULT 0,
    is_legacy_byte_range    INTEGER NOT NULL DEFAULT 0,
    share_reservation       INTEGER NOT NULL DEFAULT 0,
    acquired_at             TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    server_epoch            INTEGER NOT NULL DEFAULT 0,
    lease_key               BLOB,
    lease_state             INTEGER NOT NULL DEFAULT 0,
    lease_epoch             INTEGER,
    break_to_state          INTEGER,
    breaking_to_required    INTEGER,
    breaking                INTEGER,
    parent_lease_key        BLOB,
    is_directory            INTEGER,
    is_traditional_oplock   INTEGER,
    delegation_id           TEXT,
    deleg_type              INTEGER,
    deleg_breaking          INTEGER,
    deleg_recalled          INTEGER,
    deleg_revoked           INTEGER,
    deleg_notification_mask INTEGER,
    break_started           TIMESTAMP
);

CREATE INDEX idx_locks_client_id ON locks(client_id);
CREATE INDEX idx_locks_file_id ON locks(file_id);
CREATE INDEX idx_locks_share_name ON locks(share_name);

-- ---------------------------------------------------------------------------
-- nsm_client_registrations: NLM/NSM client persistence.
-- ---------------------------------------------------------------------------
CREATE TABLE nsm_client_registrations (
    client_id     TEXT PRIMARY KEY,
    mon_name      TEXT NOT NULL,
    priv          BLOB NOT NULL,
    callback_host TEXT NOT NULL,
    callback_prog INTEGER NOT NULL,
    callback_vers INTEGER NOT NULL,
    callback_proc INTEGER NOT NULL,
    registered_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    server_epoch  INTEGER NOT NULL,

    CONSTRAINT valid_priv CHECK (length(priv) = 16),
    CONSTRAINT valid_callback_prog CHECK (callback_prog >= 0),
    CONSTRAINT valid_callback_vers CHECK (callback_vers >= 0),
    CONSTRAINT valid_callback_proc CHECK (callback_proc >= 0)
);

CREATE INDEX idx_nsm_mon_name ON nsm_client_registrations(mon_name);

-- ---------------------------------------------------------------------------
-- durable_handles: SMB3 durable/persistent handle persistence.
-- ---------------------------------------------------------------------------
CREATE TABLE durable_handles (
    id                   TEXT PRIMARY KEY,
    file_id              BLOB NOT NULL,
    path                 TEXT NOT NULL DEFAULT '',
    share_name           TEXT NOT NULL,
    desired_access       INTEGER NOT NULL DEFAULT 0,
    share_access         INTEGER NOT NULL DEFAULT 0,
    create_options       INTEGER NOT NULL DEFAULT 0,
    metadata_handle      BLOB NOT NULL,
    payload_id           TEXT,
    oplock_level         INTEGER NOT NULL DEFAULT 0,
    lease_key            BLOB,
    lease_state          INTEGER NOT NULL DEFAULT 0,
    create_guid          BLOB,
    app_instance_id      BLOB,
    username             TEXT NOT NULL DEFAULT '',
    session_key_hash     BLOB NOT NULL,
    is_v2                INTEGER NOT NULL DEFAULT 0,
    created_at           TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    disconnected_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    timeout_ms           INTEGER NOT NULL DEFAULT 60000,
    server_start_time    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    delete_pending       INTEGER NOT NULL DEFAULT 0,
    parent_handle        BLOB,
    file_name            TEXT,
    is_directory         INTEGER NOT NULL DEFAULT 0,
    granted_access       INTEGER,
    position_info        INTEGER,
    original_file_id     BLOB,
    requested_alloc_size INTEGER,
    lease_epoch          INTEGER,
    is_persistent        INTEGER NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX idx_durable_handles_create_guid ON durable_handles(create_guid) WHERE create_guid IS NOT NULL;
CREATE INDEX idx_durable_handles_app_instance_id ON durable_handles(app_instance_id) WHERE app_instance_id IS NOT NULL;
CREATE INDEX idx_durable_handles_file_id ON durable_handles(file_id);
CREATE INDEX idx_durable_handles_share_name ON durable_handles(share_name);
CREATE INDEX idx_durable_handles_metadata_handle ON durable_handles(metadata_handle);
CREATE INDEX idx_durable_handles_disconnected_at ON durable_handles(disconnected_at);

-- ---------------------------------------------------------------------------
-- v4_client_recovery: NFSv4 client-recovery persistence.
-- ---------------------------------------------------------------------------
CREATE TABLE v4_client_recovery (
    clientid_string  TEXT PRIMARY KEY,
    clientid         INTEGER NOT NULL,
    boot_verifier    BLOB NOT NULL,
    principal        TEXT NOT NULL DEFAULT '',
    confirmed_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    server_epoch     INTEGER NOT NULL,
    reclaim_complete INTEGER NOT NULL DEFAULT 0,

    CONSTRAINT valid_boot_verifier CHECK (length(boot_verifier) = 8)
);

-- ---------------------------------------------------------------------------
-- rollup_offsets: per-payload append-log rollup position.
-- ---------------------------------------------------------------------------
CREATE TABLE rollup_offsets (
    payload_id    TEXT PRIMARY KEY,
    rollup_offset INTEGER NOT NULL DEFAULT 0
);

-- ---------------------------------------------------------------------------
-- synced_hashes: content hashes confirmed durable in the remote store.
-- ---------------------------------------------------------------------------
CREATE TABLE synced_hashes (
    hash      TEXT PRIMARY KEY,
    synced_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
