-- ============================================================================
-- WARNING — DESTRUCTIVE: this rollback DROPs the file_blocks table.
--
-- All FileBlock rows (the entire CAS object index, ref counts, sync state,
-- and cache_path bookkeeping) are PERMANENTLY DELETED. Once dropped:
--
--   - Every cached local-cache file becomes orphaned (the metadata that
--     mapped block IDs → cache paths is gone).
--   - Every CAS object on the remote becomes orphaned (no FileBlock row
--     means GC mark-phase will not record those hashes; the next sweep
--     will treat them as unreferenced and delete them after the grace
--     window).
--   - In-flight Pending / Syncing blocks lose all upload bookkeeping;
--     pending writes are silently lost.
--
-- This down migration exists for migration tooling completeness only.
-- DO NOT run on a live deployment without an explicit, tested backup +
-- restore plan for the file_blocks table.
-- ============================================================================

-- Phase 11 Plan 02: drop file_blocks table.
DROP INDEX IF EXISTS idx_file_blocks_syncing_age;
DROP INDEX IF EXISTS idx_file_blocks_unreferenced;
DROP INDEX IF EXISTS idx_file_blocks_remote;
DROP INDEX IF EXISTS idx_file_blocks_pending;
DROP INDEX IF EXISTS idx_file_blocks_hash;
DROP TABLE IF EXISTS file_blocks;
