-- Revert: drop the per-file recycle-bin metadata columns (#190 trash).

ALTER TABLE files
    DROP COLUMN IF EXISTS deleted_at,
    DROP COLUMN IF EXISTS original_path,
    DROP COLUMN IF EXISTS deleted_by;
