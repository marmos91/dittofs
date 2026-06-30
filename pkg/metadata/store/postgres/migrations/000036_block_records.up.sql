-- #1416 blocks-only storage (PR2): persist block lifecycle records and the
-- local chunk position index used by the log-blob engine.
--
-- block_records: one row per log-blob block, tracking its hash, byte length,
-- number of live (un-GC'd) chunks it contains, and its sync state.
-- live_chunk_count is BIGINT so GREATEST(0, live_chunk_count - delta) never
-- overflows in the atomic-decrement UPDATE.
--
-- local_chunk_index: maps a content hash to the position of that chunk inside
-- a local log-blob. Mirrors synced_hashes in shape (hash BYTEA primary key)
-- but points at local storage rather than remote.

CREATE TABLE IF NOT EXISTS block_records (
    block_id        TEXT    PRIMARY KEY,
    block_hash      BYTEA   NOT NULL,
    length          BIGINT  NOT NULL,
    live_chunk_count BIGINT NOT NULL DEFAULT 0,
    sync_state      SMALLINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS local_chunk_index (
    hash        BYTEA   PRIMARY KEY,
    log_blob_id TEXT    NOT NULL,
    raw_offset  BIGINT  NOT NULL,
    raw_length  BIGINT  NOT NULL
);
