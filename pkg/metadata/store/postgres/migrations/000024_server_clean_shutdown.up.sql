-- Migration: clean-shutdown marker for lock-recovery grace entry (area-4 H7).
--
-- clean_shutdown records whether the previous run terminated through a
-- fully-graceful Close(). On boot the lock subsystem enters its grace period
-- when the marker is FALSE (crash / kill -9 / power-loss) even if no locks were
-- recovered — fixing the bug where grace was entered only when recovered lock
-- state existed. The marker is stored on the existing single-row server_epoch
-- singleton (id = 1), mirroring how the server epoch is persisted.
--
-- Default FALSE: any row that predates this column, or a fresh row, is treated
-- as UNCLEAN, which is the fail-safe direction (enter grace rather than risk
-- granting a conflicting lock before a prior owner reclaims).

ALTER TABLE server_epoch ADD COLUMN IF NOT EXISTS clean_shutdown BOOLEAN NOT NULL DEFAULT FALSE;
