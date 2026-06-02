-- Persist IsPersistent on durable handles (#739 persistent-open).
--
-- An SMB3 persistent durable handle is granted when a DH2Q create context
-- carries SMB2_DHANDLE_FLAG_PERSISTENT on a continuous-availability share
-- ([MS-SMB2] 2.2.13.2.11 / 3.3.5.9.10). It is durable + persistent: the
-- distinction from a plain durable handle is that the DH2Q response echoes
-- the PERSISTENT flag. On a durable disconnect this must be persisted so the
-- reconnect re-reports persistent_open (the open stays persistent across the
-- disconnect).
--
-- The memory backend round-trips this field via struct copy and badger picks
-- it up automatically via JSON; only the postgres profile needs an explicit
-- column. Stored as BOOLEAN. Pre-existing rows default to false, which the
-- reconnect handler treats as a plain durable handle.

ALTER TABLE durable_handles
    ADD COLUMN IF NOT EXISTS is_persistent BOOLEAN NOT NULL DEFAULT FALSE;
