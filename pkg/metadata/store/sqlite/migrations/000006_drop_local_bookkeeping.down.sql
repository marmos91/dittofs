-- Recreate the dropped local bookkeeping tables (reversibility). DDL copied
-- from 000003 (local_chunk_index) and 000001 (rollup_offsets).
CREATE TABLE local_chunk_index (
    hash        BLOB PRIMARY KEY,
    log_blob_id TEXT NOT NULL,
    raw_offset  INTEGER NOT NULL,
    raw_length  INTEGER NOT NULL
);

CREATE TABLE rollup_offsets (
    payload_id    TEXT PRIMARY KEY,
    rollup_offset INTEGER NOT NULL DEFAULT 0
);
