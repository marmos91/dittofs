-- Migration: byte-range lock recovery discriminators.
--
-- is_zero_byte marks an SMB2 zero-byte byte-range lock (byte_length = 0 but
-- NOT "to EOF"). Without it a restored zero-byte lock would be treated as
-- unbounded (NFS to-EOF semantics) and produce wrong conflict checks.
--
-- is_legacy_byte_range marks a record persisted via the SMB byte-range path
-- (Manager.Lock) whose in-memory home is the legacy locks map, distinct from
-- NLM/NFSv4 unified locks. RestoreLocks uses it to route records back to the
-- correct map after a restart.

ALTER TABLE locks ADD COLUMN IF NOT EXISTS is_zero_byte BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE locks ADD COLUMN IF NOT EXISTS is_legacy_byte_range BOOLEAN NOT NULL DEFAULT FALSE;
