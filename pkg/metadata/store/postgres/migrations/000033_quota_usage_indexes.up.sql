-- Per-identity quota usage accounting indexes (#1151).
--
-- Per-user and per-group quotas track bytes + file-count for regular files,
-- charged to the file owner (inodes.uid / inodes.gid). The metadata store seeds
-- an in-memory per-identity usage cache at startup with two aggregate scans:
--
--   SELECT uid, SUM(size), COUNT(*) FROM inodes WHERE file_type = 0 GROUP BY uid
--   SELECT gid, SUM(size), COUNT(*) FROM inodes WHERE file_type = 0 GROUP BY gid
--
-- These indexes keep that seed (and any future owner-scoped query) cheap on
-- large datasets rather than forcing a full sequential scan + sort. The inodes
-- table is the single source of truth for usage; the cache is reconstructed
-- from it on every open, so no separate counter table is needed.
--
-- Runs after 000032 renamed files -> inodes.

CREATE INDEX IF NOT EXISTS idx_inodes_uid ON inodes(uid) WHERE file_type = 0;
CREATE INDEX IF NOT EXISTS idx_inodes_gid ON inodes(gid) WHERE file_type = 0;
