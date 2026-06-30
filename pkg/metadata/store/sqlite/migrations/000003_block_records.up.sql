-- #1416 blocks-only storage (PR2): block record lifecycle and local chunk index.
--
-- block_records tracks each log-blob block committed to the local store:
-- its content hash, byte length, how many live (not-yet-GC'd) chunks refer to
-- it, and the sync state (pending/syncing/synced enum — mirrors block.BlockState).
--
-- local_chunk_index maps a content hash to the position of that chunk inside
-- its local log-blob file.  It is the read-path complement to block_records:
-- given a hash the engine looks up its log-blob + byte range here.

CREATE TABLE block_records (
    block_id         TEXT PRIMARY KEY,
    block_hash       BLOB NOT NULL,
    length           INTEGER NOT NULL,
    live_chunk_count INTEGER NOT NULL,
    sync_state       INTEGER NOT NULL
);

CREATE TABLE local_chunk_index (
    hash        BLOB PRIMARY KEY,
    log_blob_id TEXT NOT NULL,
    raw_offset  INTEGER NOT NULL,
    raw_length  INTEGER NOT NULL
);
