-- Restore the original POSIX-only valid_mode upper bound.
ALTER TABLE files DROP CONSTRAINT IF EXISTS valid_mode;
ALTER TABLE files ADD CONSTRAINT valid_mode CHECK (mode >= 0 AND mode <= 4095);
