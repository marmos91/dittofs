-- Remove hidden column

DROP INDEX IF EXISTS idx_files_hidden;
ALTER TABLE files DROP COLUMN IF EXISTS hidden;
