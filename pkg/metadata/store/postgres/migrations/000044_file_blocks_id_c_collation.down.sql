-- Return file_blocks.id to the database's default collation. ListFileChunks
-- still returns the right rows afterwards — it pins COLLATE "C" on the
-- comparison — but the primary-key btree can no longer serve the range, so the
-- per-payload lookup goes back to scanning the whole table.
ALTER TABLE file_blocks ALTER COLUMN id TYPE VARCHAR(255) COLLATE pg_catalog."default";
