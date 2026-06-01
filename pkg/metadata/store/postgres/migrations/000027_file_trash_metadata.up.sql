-- Per-file recycle-bin metadata (#190 trash / recycle bin).
--
-- When a share has trash enabled, an unlink moves the node into the share's
-- #recycle directory instead of deleting it. Three nullable columns record
-- the recycle event on the moved node's root so the reaper and `dfsctl trash
-- list` can enumerate bin entries without a side table, and restore knows
-- where the file came from:
--   deleted_at    -- recycle timestamp as BIGINT unix-nanoseconds, matching the
--                    other file timestamps (atime/mtime/ctime/creation_time,
--                    see 000023_file_timestamps_nanos): NULL for live nodes,
--                    drives retention. Nanos avoid the TIMESTAMPTZ microsecond
--                    truncation so postgres round-trips on par with memory/badger.
--   original_path -- share-relative path before recycling; default restore dest
--   deleted_by    -- principal that recycled the node (display only)
--
-- The memory backend carries these via the in-process struct and badger via
-- JSON; only postgres needs explicit columns. Pre-existing rows default to
-- NULL/'' which the code treats as "live node".

ALTER TABLE files
    ADD COLUMN IF NOT EXISTS deleted_at    BIGINT,
    ADD COLUMN IF NOT EXISTS original_path TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS deleted_by    TEXT NOT NULL DEFAULT '';
