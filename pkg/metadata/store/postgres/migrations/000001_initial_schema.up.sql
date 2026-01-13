-- Initial schema for DittoFS PostgreSQL metadata store
-- Consolidated migration including all schema features:
-- - Core tables (files, parent_child_map, link_counts, shares, etc.)
-- - Hard link support (no unique constraint on child_id)
-- - Creation time for SMB/NFSv4 compatibility
-- - Hidden file attribute for SMB/Windows support
-- - nlink column for fast GETATTR
-- - Path hash indexing for long paths (>2704 bytes)
-- - Partial unique constraint for POSIX compliance (orphaned files)
-- - Share options JSONB column

-- Enable UUID extension if not already enabled
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- files: Core file and directory metadata
CREATE TABLE files (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    share_name    TEXT NOT NULL,
    path          TEXT NOT NULL,
    path_hash     TEXT NOT NULL,              -- MD5 hash of path for indexing (solves btree 2704 byte limit)
    file_type     SMALLINT NOT NULL,          -- 1=file, 2=dir, 3=symlink, 4=char, 5=block, 6=fifo, 7=socket
    mode          INTEGER NOT NULL,           -- Unix permission bits
    uid           INTEGER NOT NULL,
    gid           INTEGER NOT NULL,
    size          BIGINT NOT NULL DEFAULT 0,
    nlink         INTEGER NOT NULL DEFAULT 1, -- Hard link count for fast GETATTR
    atime         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    mtime         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ctime         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    creation_time TIMESTAMPTZ NOT NULL DEFAULT NOW(), -- Actual file creation time (birth time) for SMB/NFSv4
    content_id    TEXT,                       -- Reference to content store
    link_target   TEXT,                       -- For symlinks only
    device_major  INTEGER,                    -- For device files
    device_minor  INTEGER,                    -- For device files
    hidden        BOOLEAN NOT NULL DEFAULT FALSE, -- SMB/Windows hidden file attribute
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT valid_file_type CHECK (file_type BETWEEN 0 AND 6),
    CONSTRAINT valid_mode CHECK (mode >= 0 AND mode <= 4095),  -- 0o7777
    CONSTRAINT valid_uid CHECK (uid >= 0),
    CONSTRAINT valid_gid CHECK (gid >= 0),
    CONSTRAINT valid_size CHECK (size >= 0),
    CONSTRAINT valid_nlink CHECK (nlink >= 0)
);

-- Indexes for files table
CREATE INDEX idx_files_share_name ON files(share_name);
CREATE INDEX idx_files_content_id ON files(content_id) WHERE content_id IS NOT NULL;
CREATE INDEX idx_files_updated_at ON files(updated_at);
CREATE INDEX idx_files_hidden ON files(hidden) WHERE hidden = TRUE;

-- Partial unique index - only active files participate in uniqueness
-- This allows orphaned files (nlink=0) to coexist with new files at the same path
-- Required for POSIX compliance: fstat() on open fd after unlink() should return nlink=0
CREATE UNIQUE INDEX unique_share_path_hash_active ON files(share_name, path_hash) WHERE nlink > 0;

-- Non-unique index for general lookups (includes orphaned files)
CREATE INDEX idx_files_share_path_hash ON files(share_name, path_hash);

-- parent_child_map: Directory hierarchy for fast parent/child lookups
-- Note: No unique constraint on child_id to support hard links
CREATE TABLE parent_child_map (
    parent_id   UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    child_id    UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    child_name  TEXT NOT NULL,

    PRIMARY KEY (parent_id, child_name),
    CONSTRAINT non_empty_child_name CHECK (length(child_name) > 0)
);

-- Indexes for parent_child_map
CREATE INDEX idx_parent_child_map_parent ON parent_child_map(parent_id);
CREATE INDEX idx_parent_child_map_child ON parent_child_map(child_id);
CREATE INDEX idx_parent_child_map_parent_name ON parent_child_map(parent_id, child_name);

-- link_counts: Hard link reference counting (legacy, nlink column is preferred)
CREATE TABLE link_counts (
    file_id     UUID PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
    link_count  INTEGER NOT NULL DEFAULT 1,

    CONSTRAINT valid_link_count CHECK (link_count >= 0)
);

-- Index for link_counts
CREATE INDEX idx_link_counts_file_id ON link_counts(file_id);

-- shares: Share configuration and root directory tracking
CREATE TABLE shares (
    share_name      TEXT PRIMARY KEY,
    root_file_id    UUID NOT NULL REFERENCES files(id),
    options         JSONB NOT NULL DEFAULT '{}'::jsonb, -- Share-specific configuration
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT non_empty_share_name CHECK (length(share_name) > 0)
);

-- pending_writes: Two-phase write protocol tracking
CREATE TABLE pending_writes (
    operation_id    TEXT PRIMARY KEY,
    file_id         UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    new_size        BIGINT NOT NULL,
    new_mtime       TIMESTAMPTZ NOT NULL,
    content_id      TEXT NOT NULL,
    pre_write_attr  JSONB,           -- Snapshot of FileAttr before write
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT valid_operation_id CHECK (length(operation_id) > 0),
    CONSTRAINT valid_new_size CHECK (new_size >= 0)
);

-- Indexes for pending_writes
CREATE INDEX idx_pending_writes_file_id ON pending_writes(file_id);
CREATE INDEX idx_pending_writes_created_at ON pending_writes(created_at);

-- server_config: Server-wide configuration storage (singleton table)
CREATE TABLE server_config (
    id          INTEGER PRIMARY KEY DEFAULT 1,
    config      JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT singleton_check CHECK (id = 1)
);

-- Insert default server config
INSERT INTO server_config (id, config) VALUES (1, '{}'::jsonb);

-- filesystem_capabilities: Static filesystem capabilities (singleton table)
CREATE TABLE filesystem_capabilities (
    id                      INTEGER PRIMARY KEY DEFAULT 1,
    max_read_size           BIGINT NOT NULL,
    preferred_read_size     BIGINT NOT NULL,
    max_write_size          BIGINT NOT NULL,
    preferred_write_size    BIGINT NOT NULL,
    max_file_size           BIGINT NOT NULL,
    max_filename_len        INTEGER NOT NULL,
    max_path_len            INTEGER NOT NULL,
    max_hard_link_count     INTEGER NOT NULL,
    supports_hard_links     BOOLEAN NOT NULL,
    supports_symlinks       BOOLEAN NOT NULL,
    case_sensitive          BOOLEAN NOT NULL,
    case_preserving         BOOLEAN NOT NULL,
    supports_acls           BOOLEAN NOT NULL,
    time_resolution         BIGINT NOT NULL,

    CONSTRAINT singleton_check CHECK (id = 1),
    CONSTRAINT valid_sizes CHECK (
        max_read_size > 0 AND
        preferred_read_size > 0 AND
        max_write_size > 0 AND
        preferred_write_size > 0 AND
        max_file_size > 0 AND
        max_filename_len > 0 AND
        max_path_len > 0 AND
        max_hard_link_count >= 0 AND
        time_resolution > 0
    )
);

-- Function to update updated_at timestamp automatically
CREATE OR REPLACE FUNCTION update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ language 'plpgsql';

-- Trigger for files table
CREATE TRIGGER update_files_updated_at BEFORE UPDATE ON files
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- Trigger for server_config table
CREATE TRIGGER update_server_config_updated_at BEFORE UPDATE ON server_config
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- Function to automatically maintain path_hash on insert/update
CREATE OR REPLACE FUNCTION update_path_hash()
RETURNS TRIGGER AS $$
BEGIN
    NEW.path_hash = md5(NEW.path);
    RETURN NEW;
END;
$$ language 'plpgsql';

-- Trigger for path_hash maintenance
CREATE TRIGGER files_path_hash_trigger
    BEFORE INSERT OR UPDATE OF path ON files
    FOR EACH ROW EXECUTE FUNCTION update_path_hash();
