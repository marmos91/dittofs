ALTER TABLE file_block_refs DROP COLUMN IF EXISTS start_offset;
ALTER TABLE file_blocks DROP COLUMN IF EXISTS start_offset;
