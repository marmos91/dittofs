-- Remove the dead FileChunk.BlockStoreKey column. No current-era code writes a
-- non-empty value (only DB scan read-back echoed persisted values), and the
-- legacy IsRemote() dual-read that consumed it has been simplified to
-- State == Remote. No index references the column.
ALTER TABLE file_blocks DROP COLUMN block_store_key;
