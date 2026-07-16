-- Re-add the dropped block_store_key column (reversibility).
-- DDL copied from 000010_file_blocks.up.sql.
ALTER TABLE file_blocks ADD COLUMN IF NOT EXISTS block_store_key TEXT;
