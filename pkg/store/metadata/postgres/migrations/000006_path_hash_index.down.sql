-- Rollback: Remove path_hash approach, restore original path-based indexing
-- WARNING: This will fail if any paths exceed 2704 bytes (PostgreSQL btree limit)

-- Step 1: Drop trigger and function
DROP TRIGGER IF EXISTS files_path_hash_trigger ON files;
DROP FUNCTION IF EXISTS update_path_hash();

-- Step 2: Drop hash-based constraint and index
DROP INDEX IF EXISTS idx_files_share_path_hash;
ALTER TABLE files DROP CONSTRAINT IF EXISTS unique_share_path_hash;

-- Step 3: Restore original constraint and index (may fail for long paths!)
ALTER TABLE files ADD CONSTRAINT unique_share_path UNIQUE(share_name, path);
CREATE INDEX idx_files_share_path ON files(share_name, path);

-- Step 4: Remove path_hash column
ALTER TABLE files DROP COLUMN IF EXISTS path_hash;
