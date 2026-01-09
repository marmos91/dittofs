-- Rollback: Restore full unique constraint on path_hash
-- WARNING: This will fail if there are orphaned files (nlink=0) with duplicate paths

-- Step 1: Drop the partial unique index
DROP INDEX IF EXISTS unique_share_path_hash_active;

-- Step 2: Drop the general lookup index
DROP INDEX IF EXISTS idx_files_share_path_hash;

-- Step 3: Restore the full unique constraint
ALTER TABLE files ADD CONSTRAINT unique_share_path_hash UNIQUE(share_name, path_hash);

-- Step 4: Restore the lookup index
CREATE INDEX idx_files_share_path_hash ON files(share_name, path_hash);
