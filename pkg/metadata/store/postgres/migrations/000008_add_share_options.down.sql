-- Remove options column from shares table
ALTER TABLE shares DROP COLUMN IF EXISTS options;
