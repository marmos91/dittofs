-- Rename partial indexes to match new block state terminology
-- State values: 0=Dirty, 1=Local (was Sealed), 2=Syncing (was Uploading), 3=Remote (was Uploaded)
ALTER INDEX IF EXISTS idx_file_blocks_pending RENAME TO idx_file_blocks_local;
ALTER INDEX IF EXISTS idx_file_blocks_evictable RENAME TO idx_file_blocks_remote;
