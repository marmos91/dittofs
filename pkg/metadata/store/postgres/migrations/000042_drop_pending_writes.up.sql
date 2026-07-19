-- Remove the dead pending_writes table. It was provisioned for a two-phase
-- write protocol that was never implemented: no code inserts, selects, or
-- updates rows, so the table (and its two indexes) has always been empty.
-- Dropping the table drops idx_pending_writes_file_id and
-- idx_pending_writes_created_at with it.
DROP TABLE IF EXISTS pending_writes;
