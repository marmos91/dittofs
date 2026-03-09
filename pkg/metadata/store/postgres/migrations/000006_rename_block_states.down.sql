-- Revert index renames back to original terminology
ALTER INDEX IF EXISTS idx_file_blocks_local RENAME TO idx_file_blocks_pending;
ALTER INDEX IF EXISTS idx_file_blocks_remote RENAME TO idx_file_blocks_evictable;
