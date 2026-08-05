-- Drop the UNIQUE constraint on durable_handles.file_id.
--
-- file_id identifies the file, not the open: the volatile half is zeroed
-- before it is stored, so every durable handle on a file carries the same
-- value. A file held open more than once therefore needs one row per handle,
-- which the original 000005 UNIQUE index rejected outright — the second open
-- failed with "duplicate key value violates unique constraint" and never
-- persisted its durable state.
--
-- Replaced with a plain index: the column is still the lookup key for
-- reconnect, it just no longer claims to identify at most one row.
DROP INDEX IF EXISTS idx_durable_handles_file_id;
CREATE INDEX IF NOT EXISTS idx_durable_handles_file_id ON durable_handles(file_id);
