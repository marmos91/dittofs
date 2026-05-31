-- Persist RequestedAllocSize on durable handles (#792 follow-up to #875).
--
-- A CREATE may carry an SMB2_CREATE_ALLOCATION_SIZE ("AlSi") create context
-- ([MS-SMB2] 2.2.13.2.2) requesting an initial allocation. DittoFS does not
-- preallocate; the value only raises the (cluster-aligned) AllocationSize
-- reported in the CREATE response. The reservation is per-handle in-memory
-- state, so it must be persisted to survive a durable disconnect — otherwise
-- the durable-reconnect CREATE response drops out.alloc_size back to the
-- file's bare size, reporting a smaller value than the original open
-- (smb2.durable-open.alloc-size reopen checks).
--
-- The memory backend already round-trips this field via cloneDurableHandle and
-- badger picks it up automatically via JSON; only the postgres profile needs
-- an explicit column.
--
-- Stored as BIGINT (signed int64) reinterpreting the uint64 bit pattern, the
-- same encoding used for position_info. Pre-existing rows default to 0, which
-- the reconnect handler treats as "no reservation".

ALTER TABLE durable_handles
    ADD COLUMN IF NOT EXISTS requested_alloc_size BIGINT NOT NULL DEFAULT 0;
