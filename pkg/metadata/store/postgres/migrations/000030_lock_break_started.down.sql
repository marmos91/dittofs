-- Revert: drop the locks break_started column (#1080 audit fix).

ALTER TABLE locks
    DROP COLUMN IF EXISTS break_started;
