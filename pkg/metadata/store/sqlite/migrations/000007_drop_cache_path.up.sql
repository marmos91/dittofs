-- Post-journal cleanup: FileChunk.LocalPath is dead — no production constructor
-- sets it and no reader consumes it (the local store is journal-backed). The
-- cache_path column only round-tripped a value that was always empty. Drop it.
-- SQLite refuses to drop a column referenced by an index predicate, so drop the
-- two partial indexes (cache_path IS NOT NULL, never true post-journal) first.
DROP INDEX IF EXISTS idx_file_blocks_pending;
DROP INDEX IF EXISTS idx_file_blocks_remote;
ALTER TABLE file_blocks DROP COLUMN cache_path;
