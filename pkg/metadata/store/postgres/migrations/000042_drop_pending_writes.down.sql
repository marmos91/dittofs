-- Re-create the dropped pending_writes table and its indexes (reversibility).
-- DDL copied from 000001_initial_schema.up.sql.
CREATE TABLE pending_writes (
    operation_id    TEXT PRIMARY KEY,
    file_id         UUID NOT NULL REFERENCES inodes(id) ON DELETE CASCADE,
    new_size        BIGINT NOT NULL,
    new_mtime       TIMESTAMPTZ NOT NULL,
    content_id      TEXT NOT NULL,
    pre_write_attr  JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT valid_operation_id CHECK (length(operation_id) > 0),
    CONSTRAINT valid_new_size CHECK (new_size >= 0)
);

CREATE INDEX idx_pending_writes_file_id ON pending_writes(file_id);
CREATE INDEX idx_pending_writes_created_at ON pending_writes(created_at);
