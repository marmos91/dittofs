-- Persist BreakStarted on locks (#1080 audit fix).
--
-- PersistedLock gained a BreakStarted timestamp recording when a lease break
-- was initiated (Breaking=true). The memory and badger backends round-trip it
-- via JSON automatically; the postgres backend stores locks as columns and so
-- silently dropped it on PutLock, failing the LockPersistenceConformance
-- field-drop guard. This column closes that gap.
--
-- The column is nullable: legacy rows and locks with no in-flight break carry
-- a zero BreakStarted, which the backend stores as NULL and loads back as the
-- zero time.

ALTER TABLE locks
    ADD COLUMN IF NOT EXISTS break_started TIMESTAMPTZ;
