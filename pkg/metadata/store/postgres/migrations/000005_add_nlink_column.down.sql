-- Remove nlink column from files table
ALTER TABLE files DROP COLUMN IF EXISTS nlink;
