-- ============================================================================
-- WARNING - DESTRUCTIVE: drops file_block_refs.
--
-- Every FileAttr.Blocks []BlockRef payload is permanently deleted.
-- Files written post-Phase-12 will revert to dual-read shim semantics
-- on next read (zero-fill where chunks were authoritative). This is
-- safe for the v0.15.0 dual-read window (D-24) but operationally
-- equivalent to a full re-chunk for any file written under Phase 12.
--
-- Per D-07 this is forward-only at the operational level — only run
-- pre-deploy or with an explicit Phase-14 migration plan.
-- ============================================================================

DROP INDEX IF EXISTS idx_file_block_refs_hash;
DROP TABLE IF EXISTS file_block_refs;
