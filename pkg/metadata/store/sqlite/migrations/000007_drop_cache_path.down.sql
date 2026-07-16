-- Re-add the dropped cache_path column and its partial indexes (reversibility).
-- DDL copied from 000001_initial_schema.up.sql.
ALTER TABLE file_blocks ADD COLUMN cache_path TEXT;
CREATE INDEX idx_file_blocks_pending ON file_blocks(created_at) WHERE state = 0 AND cache_path IS NOT NULL;
CREATE INDEX idx_file_blocks_remote ON file_blocks(last_access) WHERE state = 2 AND cache_path IS NOT NULL;
