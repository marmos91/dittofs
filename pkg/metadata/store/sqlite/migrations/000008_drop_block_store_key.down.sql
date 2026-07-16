-- Re-add the dropped block_store_key column (reversibility).
-- DDL copied from 000001_initial_schema.up.sql.
ALTER TABLE file_blocks ADD COLUMN block_store_key TEXT;
