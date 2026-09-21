-- Durable per-identity usage counters.
--
-- Maintained inside the same transaction as the inode rows they account for, so
-- the two can never disagree and an open reads them instead of re-aggregating
-- the whole inodes table. Only per-identity buckets are stored: the per-share
-- total is derived from the user-scope rows, since every regular file has
-- exactly one owner uid and those rows already partition the share.
--
-- scope mirrors metadata.QuotaScope: 0 = user (uid), 1 = group (gid).
-- Values are stored raw and signed; clamping at zero belongs to the reader.
CREATE TABLE IF NOT EXISTS quota_usage (
    share_name  TEXT     NOT NULL,
    scope       SMALLINT NOT NULL,
    identity_id BIGINT   NOT NULL,
    bytes       BIGINT   NOT NULL DEFAULT 0,
    files       BIGINT   NOT NULL DEFAULT 0,
    PRIMARY KEY (share_name, scope, identity_id)
);

-- Backfill from the rows themselves, so a database that predates the counters
-- comes up with counters already accounting for every inode. This aggregate is
-- the one the store used to run on every open; running it here makes it a
-- once-per-database cost instead.
-- file_type 0 is metadata.FileTypeRegular; nlink > 0 excludes an inode that is
-- unlinked but still held open, which keeps its row but none of the bytes.
INSERT INTO quota_usage (share_name, scope, identity_id, bytes, files)
SELECT share_name, 0, uid, COALESCE(SUM(size), 0), COUNT(*)
FROM inodes
WHERE file_type = 0 AND nlink > 0
GROUP BY share_name, uid
ON CONFLICT (share_name, scope, identity_id) DO NOTHING;

INSERT INTO quota_usage (share_name, scope, identity_id, bytes, files)
SELECT share_name, 1, gid, COALESCE(SUM(size), 0), COUNT(*)
FROM inodes
WHERE file_type = 0 AND nlink > 0
GROUP BY share_name, gid
ON CONFLICT (share_name, scope, identity_id) DO NOTHING;
