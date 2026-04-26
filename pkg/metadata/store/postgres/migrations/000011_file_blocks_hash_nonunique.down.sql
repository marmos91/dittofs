-- Phase 11 WR-4-01 down: restore the (broken) UNIQUE constraint.
--
-- WARNING: this rollback re-introduces the cross-row hash uniqueness
-- bug — engine.uploadOne dedup short-circuits will start failing on
-- legitimate cross-file hash collisions (e.g. all-zero VM blocks),
-- leaving FileBlocks in Syncing and leaking donor RefCounts.
-- Down only intended for migration tooling completeness; do NOT run on
-- a live deployment.

DROP INDEX IF EXISTS idx_file_blocks_hash;

CREATE UNIQUE INDEX IF NOT EXISTS idx_file_blocks_hash
    ON file_blocks(hash) WHERE hash IS NOT NULL;
