-- Phase 11 Plan 02: file_blocks table for the FileBlockStore.
--
-- Pre-Phase-11 the table was created lazily by an in-process bootstrap
-- (`fileBlocksTableMigration` in pkg/metadata/store/postgres/objects.go) that
-- was never wired to the migration runner. This migration codifies the schema
-- and aligns the state column with the Phase 11 three-state lifecycle:
--   0 = Pending   (RefCount>=1, not yet uploaded — was Dirty/Local pre-Phase-11)
--   1 = Syncing   (claim batch flipped State, upload in flight)
--   2 = Remote    (PUT-success + metadata-txn-success — D-11)
--
-- last_sync_attempt_at is the timestamp the syncer's claim batch stamped on
-- the row when flipping it to Syncing. The restart-recovery janitor (D-14)
-- compares it against syncer.claim_timeout to decide whether a Syncing row
-- has been abandoned by a previous syncer instance.

CREATE TABLE IF NOT EXISTS file_blocks (
    id                     VARCHAR(255) PRIMARY KEY,
    hash                   VARCHAR(80),
    data_size              INTEGER NOT NULL DEFAULT 0,
    cache_path             TEXT,
    block_store_key        TEXT,
    ref_count              INTEGER NOT NULL DEFAULT 1,
    last_access            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    state                  SMALLINT NOT NULL DEFAULT 0, -- 0=Pending, 1=Syncing, 2=Remote (Phase 11 STATE-01)
    last_sync_attempt_at   TIMESTAMPTZ                  -- Phase 11 D-13/D-14 — null when never claimed
);

-- WR-4-01: idx_file_blocks_hash is intentionally NOT a UNIQUE index. The
-- dedup short-circuit (engine.uploadOne) produces multiple FileBlock rows
-- sharing the same ContentHash whenever two file regions hash-match. A
-- UNIQUE constraint would reject the second writer and leak the donor's
-- RefCount. See 000011_file_blocks_hash_nonunique.up.sql and the
-- FileBlockStore.PutFileBlock contract in pkg/blockstore/store.go.
CREATE INDEX IF NOT EXISTS idx_file_blocks_hash
    ON file_blocks(hash) WHERE hash IS NOT NULL;

-- Pending+local-cache rows are what the syncer claims; the partial index
-- accelerates ListLocalBlocks under load.
CREATE INDEX IF NOT EXISTS idx_file_blocks_pending
    ON file_blocks(created_at)
    WHERE state = 0 AND cache_path IS NOT NULL;

-- Remote-cached rows feed the LRU eviction path.
CREATE INDEX IF NOT EXISTS idx_file_blocks_remote
    ON file_blocks(last_access)
    WHERE state = 2 AND cache_path IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_file_blocks_unreferenced
    ON file_blocks(id) WHERE ref_count = 0;

-- Janitor index: Syncing rows ordered by claim time so the restart-recovery
-- pass can prune stale entries with a bounded scan.
CREATE INDEX IF NOT EXISTS idx_file_blocks_syncing_age
    ON file_blocks(last_sync_attempt_at)
    WHERE state = 1;
