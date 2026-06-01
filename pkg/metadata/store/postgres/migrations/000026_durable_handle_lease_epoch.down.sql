-- Revert: drop the durable-handle lease_epoch column (#739 lock-lease).

ALTER TABLE durable_handles
    DROP COLUMN IF EXISTS lease_epoch;
