-- Rename files -> inodes and drop the path-as-key columns (#1166, part 1).
--
-- The `files` row conflated inode identity/attributes (shared across hard
-- links) with one canonical `path`/`path_hash` (only one of N names). That
-- single-path assumption produced a recurring class of postgres-only bugs
-- (#1160 rename-overwrite, #190 recycle "already exists") because a multiply-
-- linked inode has only ONE path column that goes stale when the matching name
-- is removed. #1165 (migration 000031) dropped the unique_share_path_hash_active
-- index as a stopgap; this migration does the proper decoupling.
--
-- The namespace's sole source of truth is parent_child_map(parent_id,
-- child_name -> child_id). Full paths are derived on demand from it (recursive
-- walk in GetFile); the denormalized path/path_hash columns are no longer
-- needed and are removed here. The memory and badger backends never carried a
-- canonical path on the inode; this aligns postgres with them.
--
-- content_id / content_id_hash are kept AS-IS: they key file_blocks.id and are
-- consumed by GetFileByPayloadID / the flusher. Decoupling content_id from the
-- path is a later slice (#1166 part 3); this migration does not touch its
-- values.
--
-- Handle stability (invariant #3) is preserved: a file handle is
-- shareName:UUID = inodes.id, and a table rename does not change the UUID.

-- Rename the table. PostgreSQL keeps existing indexes, triggers, constraints
-- and foreign keys (parent_child_map, link_counts, shares, pending_writes, ...)
-- attached to the renamed table automatically, so all FKs that referenced
-- files(id) now reference inodes(id) with no further action.
ALTER TABLE files RENAME TO inodes;

-- Drop the path-derived index and the trigger that maintained path_hash, then
-- the columns themselves. unique_share_path_hash_active was already dropped in
-- 000031.
DROP TRIGGER IF EXISTS files_path_hash_trigger ON inodes;
DROP FUNCTION IF EXISTS update_path_hash();
DROP INDEX IF EXISTS idx_files_share_path_hash;

ALTER TABLE inodes DROP COLUMN IF EXISTS path_hash;
ALTER TABLE inodes DROP COLUMN IF EXISTS path;

-- Rename the indexes/triggers whose names embed the old "files" table name so
-- the schema stays self-describing. Renames are metadata-only (no rewrite) and
-- leave behavior identical. content_id_hash itself is unchanged.
ALTER INDEX IF EXISTS idx_files_content_id_hash RENAME TO idx_inodes_content_id_hash;
ALTER INDEX IF EXISTS idx_files_share_name RENAME TO idx_inodes_share_name;
ALTER INDEX IF EXISTS idx_files_updated_at RENAME TO idx_inodes_updated_at;
ALTER INDEX IF EXISTS idx_files_hidden RENAME TO idx_inodes_hidden;
ALTER INDEX IF EXISTS idx_files_has_acl RENAME TO idx_inodes_has_acl;
ALTER INDEX IF EXISTS files_object_id_idx RENAME TO inodes_object_id_idx;

ALTER TRIGGER update_files_updated_at ON inodes RENAME TO update_inodes_updated_at;
ALTER TRIGGER files_content_id_hash_trigger ON inodes RENAME TO inodes_content_id_hash_trigger;
