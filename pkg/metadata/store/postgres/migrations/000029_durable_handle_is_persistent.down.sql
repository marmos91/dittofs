-- Revert: drop the durable-handle is_persistent column (#739 persistent-open).

ALTER TABLE durable_handles
    DROP COLUMN IF EXISTS is_persistent;
