-- Revert: rename inodes -> files and restore the path/path_hash columns (#1166).
--
-- TRADEOFF / data caveat: the up migration DROPPED the canonical `path` column.
-- That string is not recoverable verbatim — it was a denormalized cache of one
-- of an inode's names. This down migration reconstructs a WORKING path for
-- every reachable inode from parent_child_map (the authoritative namespace),
-- choosing a single deterministic name per inode (lowest child_name when an
-- inode has several hard links). The result is a valid, fully-indexed schema,
-- but for a multiply-linked inode the recovered `path` may differ from whatever
-- single name the column happened to hold before the up migration. Inodes that
-- are not reachable from any parent (e.g. orphaned/unlinked-but-open rows) get
-- '/' as a placeholder so the NOT NULL column can be populated. This is a
-- best-effort reverse: it restores schema and behavior, not the exact prior
-- string for hard-linked files.

ALTER TABLE inodes RENAME TO files;

-- Restore renamed indexes/triggers to their original "files" names.
ALTER INDEX IF EXISTS idx_inodes_content_id_hash RENAME TO idx_files_content_id_hash;
ALTER INDEX IF EXISTS idx_inodes_share_name RENAME TO idx_files_share_name;
ALTER INDEX IF EXISTS idx_inodes_updated_at RENAME TO idx_files_updated_at;
ALTER INDEX IF EXISTS idx_inodes_hidden RENAME TO idx_files_hidden;
ALTER INDEX IF EXISTS idx_inodes_has_acl RENAME TO idx_files_has_acl;
ALTER INDEX IF EXISTS inodes_object_id_idx RENAME TO files_object_id_idx;

ALTER TRIGGER update_inodes_updated_at ON files RENAME TO update_files_updated_at;
ALTER TRIGGER inodes_content_id_hash_trigger ON files RENAME TO files_content_id_hash_trigger;

-- Re-add the columns. path_hash is filled by the recreated trigger on the
-- backfill UPDATE below; add path nullable first so the table can hold rows
-- while we reconstruct, then enforce NOT NULL.
ALTER TABLE files ADD COLUMN IF NOT EXISTS path      TEXT;
ALTER TABLE files ADD COLUMN IF NOT EXISTS path_hash TEXT;

-- Recreate the path_hash maintenance function + trigger.
CREATE OR REPLACE FUNCTION update_path_hash()
RETURNS TRIGGER AS $$
BEGIN
    NEW.path_hash = md5(NEW.path);
    RETURN NEW;
END;
$$ language 'plpgsql';

CREATE TRIGGER files_path_hash_trigger
    BEFORE INSERT OR UPDATE OF path ON files
    FOR EACH ROW EXECUTE FUNCTION update_path_hash();

-- Reconstruct each inode's path from parent_child_map. The recursive CTE walks
-- DOWN from each share root (an inode with no parent edge) building paths; when
-- an inode is reachable under several names only the lexicographically smallest
-- full path is kept (DISTINCT ON ordered by path).
WITH RECURSIVE tree(id, path, depth) AS (
    -- Roots: inodes that are never a child in parent_child_map.
    SELECT f.id AS id, '/'::text AS path, 1 AS depth
    FROM files f
    WHERE NOT EXISTS (SELECT 1 FROM parent_child_map m WHERE m.child_id = f.id)

    UNION ALL

    -- depth < 4096 bounds the walk against a corrupt directed cycle in
    -- parent_child_map (the schema does not forbid one); 4096 is well above any
    -- real path depth, so legitimate trees are never truncated.
    SELECT m.child_id AS id,
           CASE WHEN t.path = '/' THEN '/' || m.child_name
                ELSE t.path || '/' || m.child_name END AS path,
           t.depth + 1
    FROM tree t
    JOIN parent_child_map m ON m.parent_id = t.id
    WHERE t.depth < 4096
),
chosen AS (
    SELECT DISTINCT ON (id) id, path
    FROM tree
    ORDER BY id, path
)
UPDATE files f
SET path = c.path
FROM chosen c
WHERE f.id = c.id;

-- Any inode the walk could not reach (orphaned/unlinked rows with no parent
-- edge that are also not a root) falls back to '/' so the column is populated.
UPDATE files SET path = '/' WHERE path IS NULL;

-- path_hash was set by the trigger on each UPDATE above; backfill defensively
-- for any row the UPDATEs skipped, then enforce NOT NULL on both columns.
UPDATE files SET path_hash = md5(path) WHERE path_hash IS NULL;

ALTER TABLE files ALTER COLUMN path      SET NOT NULL;
ALTER TABLE files ALTER COLUMN path_hash SET NOT NULL;

-- Recreate the non-unique lookup index dropped by the up migration. The
-- unique_share_path_hash_active index is intentionally NOT recreated here: it
-- was already dropped by 000031 (its own down migration owns that revert), and
-- recreating it could fail against hard-link state this very refactor enables.
CREATE INDEX IF NOT EXISTS idx_files_share_path_hash ON files(share_name, path_hash);
