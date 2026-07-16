-- Recreate the dropped local bookkeeping tables (reversibility). DDL copied
-- from 000036 (local_chunk_index) and 000009 (rollup_offsets).
CREATE TABLE IF NOT EXISTS local_chunk_index (
    hash        BYTEA   PRIMARY KEY,
    log_blob_id TEXT    NOT NULL,
    raw_offset  BIGINT  NOT NULL,
    raw_length  BIGINT  NOT NULL
);

CREATE TABLE IF NOT EXISTS rollup_offsets (
    payload_id    TEXT PRIMARY KEY,
    rollup_offset BIGINT NOT NULL,

    CONSTRAINT valid_rollup_offset CHECK (rollup_offset >= 0)
);
