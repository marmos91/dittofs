-- Migration to fix path length limitation in PostgreSQL btree indexes
-- PostgreSQL btree indexes have a 2704 byte row limit, but POSIX PATH_MAX is 4096
-- Solution: Use MD5 hash of path for indexing (32 bytes fixed size)

-- Step 1: Drop ALL existing path-related constraints and indexes first
-- Use DO block to dynamically find and drop any constraints on the path column
DO $$
DECLARE
    constraint_name TEXT;
    index_name TEXT;
BEGIN
    -- Drop all constraints that might reference the path column
    FOR constraint_name IN
        SELECT con.conname
        FROM pg_constraint con
        JOIN pg_class rel ON rel.oid = con.conrelid
        WHERE rel.relname = 'files'
        AND con.contype = 'u'  -- unique constraints
        AND EXISTS (
            SELECT 1 FROM unnest(con.conkey) AS k
            JOIN pg_attribute att ON att.attrelid = rel.oid AND att.attnum = k
            WHERE att.attname = 'path'
        )
    LOOP
        EXECUTE format('ALTER TABLE files DROP CONSTRAINT IF EXISTS %I', constraint_name);
        RAISE NOTICE 'Dropped constraint: %', constraint_name;
    END LOOP;

    -- Drop all indexes on the path column (that are not for path_hash)
    FOR index_name IN
        SELECT i.relname
        FROM pg_index idx
        JOIN pg_class i ON i.oid = idx.indexrelid
        JOIN pg_class t ON t.oid = idx.indrelid
        WHERE t.relname = 'files'
        AND EXISTS (
            SELECT 1 FROM unnest(idx.indkey) AS k
            JOIN pg_attribute att ON att.attrelid = t.oid AND att.attnum = k
            WHERE att.attname = 'path' AND att.attname != 'path_hash'
        )
    LOOP
        EXECUTE format('DROP INDEX IF EXISTS %I', index_name);
        RAISE NOTICE 'Dropped index: %', index_name;
    END LOOP;
END $$;

-- Step 2: Add path_hash column if it doesn't exist
ALTER TABLE files ADD COLUMN IF NOT EXISTS path_hash TEXT;

-- Step 3: Populate path_hash for existing rows
UPDATE files SET path_hash = md5(path) WHERE path_hash IS NULL;

-- Step 4: Set NOT NULL constraint
ALTER TABLE files ALTER COLUMN path_hash SET NOT NULL;

-- Step 5: Drop hash-based constraint/index if they exist (for re-running)
ALTER TABLE files DROP CONSTRAINT IF EXISTS unique_share_path_hash;
DROP INDEX IF EXISTS idx_files_share_path_hash;

-- Step 6: Create new unique constraint using hash
ALTER TABLE files ADD CONSTRAINT unique_share_path_hash UNIQUE(share_name, path_hash);

-- Step 7: Create new index using hash (for lookups)
CREATE INDEX IF NOT EXISTS idx_files_share_path_hash ON files(share_name, path_hash);

-- Step 8: Add trigger to automatically maintain path_hash on insert/update
CREATE OR REPLACE FUNCTION update_path_hash()
RETURNS TRIGGER AS $$
BEGIN
    NEW.path_hash = md5(NEW.path);
    RETURN NEW;
END;
$$ language 'plpgsql';

DROP TRIGGER IF EXISTS files_path_hash_trigger ON files;
CREATE TRIGGER files_path_hash_trigger
    BEFORE INSERT OR UPDATE OF path ON files
    FOR EACH ROW EXECUTE FUNCTION update_path_hash();
