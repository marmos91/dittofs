-- Migration to use partial unique constraint for path_hash
-- This allows orphaned files (nlink=0) to coexist with new files at the same path
--
-- Problem: When a file is deleted, we keep the entry with nlink=0 for POSIX compliance
-- (fstat() on an open fd after unlink() should return nlink=0, not ESTALE).
-- But the unique constraint on (share_name, path_hash) blocks creating new files
-- at the same path.
--
-- Solution: Use a partial unique index that only applies to active files (nlink > 0).
-- Orphaned files (nlink=0) are excluded from uniqueness checks.

-- Step 1: Drop the existing unique constraint
ALTER TABLE files DROP CONSTRAINT IF EXISTS unique_share_path_hash;

-- Step 2: Drop the existing index (will be replaced with partial index)
DROP INDEX IF EXISTS idx_files_share_path_hash;

-- Step 3: Create partial unique index - only active files participate in uniqueness
-- This is the key change: WHERE nlink > 0 excludes orphaned files
CREATE UNIQUE INDEX unique_share_path_hash_active
ON files(share_name, path_hash)
WHERE nlink > 0;

-- Step 4: Create non-unique index for general lookups (includes orphaned files)
-- This ensures GETATTR on orphaned handles is still fast
CREATE INDEX idx_files_share_path_hash ON files(share_name, path_hash);
