-- Persist LeaseEpoch on durable handles (#739 lock-lease).
--
-- An SMB3 lease-V2 carries an Epoch (state-change counter, [MS-SMB2]
-- 2.2.13.2.8 / 2.2.23.2) that the server increments on every lease state
-- change and echoes in the lease-response create context. The live counter
-- lives in the protocol-agnostic lock layer (OpLock.Lease.Epoch); on a
-- durable disconnect it must be persisted so the reconnect CREATE response
-- restores the same epoch the client last saw. Without it, reconnect reports
-- Epoch=0 and the next break notification leaks a stale NewEpoch
-- (smb2.durable-v2-open.lock-lease asserts lease_epoch==1 on reconnect).
--
-- The memory backend round-trips this field via cloneDurableHandle and badger
-- picks it up automatically via JSON; only the postgres profile needs an
-- explicit column. Stored as INTEGER (the epoch is a uint16). Pre-existing
-- rows default to 0, which the reconnect handler treats as "no persisted
-- epoch" and falls back to the re-granted value.

ALTER TABLE durable_handles
    ADD COLUMN IF NOT EXISTS lease_epoch INTEGER NOT NULL DEFAULT 0;
