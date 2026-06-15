-- Revert per-identity quota usage accounting indexes (#1151).

DROP INDEX IF EXISTS idx_inodes_uid;
DROP INDEX IF EXISTS idx_inodes_gid;
