-- Initial schema for DittoFS PostgreSQL metadata store
-- Creates all tables with proper indexes and constraints

-- Enable UUID extension if not already enabled
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- files: Core file and directory metadata
CREATE TABLE files (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    share_name    TEXT NOT NULL,
    path          TEXT NOT NULL,
    file_type     SMALLINT NOT NULL,    -- 1=file, 2=dir, 3=symlink, 4=char, 5=block, 6=fifo, 7=socket
    mode          INTEGER NOT NULL,     -- Unix permission bits
    uid           INTEGER NOT NULL,
    gid           INTEGER NOT NULL,
    size          BIGINT NOT NULL DEFAULT 0,
    atime         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    mtime         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ctime         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    content_id    TEXT,                 -- Reference to content store
    link_target   TEXT,                 -- For symlinks only
    device_major  INTEGER,              -- For device files
    device_minor  INTEGER,              -- For device files
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT unique_share_path UNIQUE(share_name, path),
    CONSTRAINT valid_file_type CHECK (file_type BETWEEN 1 AND 7),
    CONSTRAINT valid_mode CHECK (mode >= 0 AND mode <= 4095),  -- 0o7777
    CONSTRAINT valid_uid CHECK (uid >= 0),
    CONSTRAINT valid_gid CHECK (gid >= 0),
    CONSTRAINT valid_size CHECK (size >= 0)
);

-- Indexes for files table
CREATE INDEX idx_files_share_name ON files(share_name);
CREATE INDEX idx_files_content_id ON files(content_id) WHERE content_id IS NOT NULL;
CREATE INDEX idx_files_share_path ON files(share_name, path);
CREATE INDEX idx_files_updated_at ON files(updated_at);

-- parent_child_map: Directory hierarchy for fast parent/child lookups
CREATE TABLE parent_child_map (
    parent_id   UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    child_id    UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    child_name  TEXT NOT NULL,

    PRIMARY KEY (parent_id, child_name),
    CONSTRAINT unique_child UNIQUE (child_id),
    CONSTRAINT non_empty_child_name CHECK (length(child_name) > 0)
);

-- Indexes for parent_child_map
CREATE INDEX idx_parent_child_map_parent ON parent_child_map(parent_id);
CREATE INDEX idx_parent_child_map_child ON parent_child_map(child_id);
CREATE INDEX idx_parent_child_map_parent_name ON parent_child_map(parent_id, child_name);

-- link_counts: Hard link reference counting
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

-- Index for pending_writes
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

-- Create function to update updated_at timestamp automatically
CREATE OR REPLACE FUNCTION update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ language 'plpgsql';

-- Create trigger for files table
CREATE TRIGGER update_files_updated_at BEFORE UPDATE ON files
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- Create trigger for server_config table
CREATE TRIGGER update_server_config_updated_at BEFORE UPDATE ON server_config
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
