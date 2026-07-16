-- Post-journal cleanup: FileChunk.LocalPath is dead — no production constructor
-- sets it and no reader consumes it (the local store is journal-backed). The
-- cache_path column only round-tripped a value that was always empty. Drop it,
-- along with the two partial indexes whose predicate references it
-- (cache_path IS NOT NULL is never true post-journal, so both indexes are dead).
DROP INDEX IF EXISTS idx_file_blocks_pending;
DROP INDEX IF EXISTS idx_file_blocks_remote;
ALTER TABLE file_blocks DROP COLUMN IF EXISTS cache_path;
