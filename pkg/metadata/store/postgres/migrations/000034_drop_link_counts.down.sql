-- Rollback: recreate the legacy link_counts table and repopulate it from
-- inodes.nlink (the authoritative column after 000034).

-- Same DDL as 000001, except the FK now references inodes(id) (post-000032
-- rename) instead of files(id).
CREATE TABLE link_counts (
    file_id     UUID PRIMARY KEY REFERENCES inodes(id) ON DELETE CASCADE,
    link_count  INTEGER NOT NULL DEFAULT 1,

    CONSTRAINT valid_link_count CHECK (link_count >= 0)
);

CREATE INDEX idx_link_counts_file_id ON link_counts(file_id);

-- Repopulate from the authoritative inode column so a re-applied old code path
-- that reads link_counts sees the correct values.
INSERT INTO link_counts (file_id, link_count)
SELECT id, nlink FROM inodes;
