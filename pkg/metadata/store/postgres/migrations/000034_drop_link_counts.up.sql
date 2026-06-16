-- Drop the legacy link_counts table; inodes.nlink is the sole source of truth
-- for hard-link counts (#1166, part 4).
--
-- link_counts duplicated the count already carried by inodes.nlink (an existing
-- NOT NULL DEFAULT 1 column). SetLinkCount dual-wrote both, and every read path
-- (GetFile / ListChildren / CreateRootDirectory) LEFT JOINed link_counts purely
-- to recover a value that nlink already held. Consolidating onto inodes.nlink
-- removes the redundant table, the dual-write, and one join from the #1176
-- single-round-trip GetFile.
--
-- 000032 renamed files -> inodes; link_counts' FK followed the rename and still
-- references inodes(id) ON DELETE CASCADE. This migration is the first to touch
-- link_counts since; nothing earlier dropped or altered it.

-- Defensively reconcile any divergence into the authoritative column before the
-- table goes away. nlink and link_count were kept in sync by SetLinkCount, but
-- backfilling guarantees correctness for any row where they drifted.
UPDATE inodes i SET nlink = lc.link_count
FROM link_counts lc
WHERE lc.file_id = i.id;

DROP INDEX IF EXISTS idx_link_counts_file_id;
DROP TABLE link_counts;
