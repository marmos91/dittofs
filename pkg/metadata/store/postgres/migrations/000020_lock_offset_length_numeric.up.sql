-- Widen byte_offset/byte_length to NUMERIC(20) so they hold the full uint64
-- range. NFSv4 expresses an unbounded byte-range lock as
-- Offset/Length = 0xFFFFFFFFFFFFFFFF (18446744073709551615), and SMB permits
-- high-bit offsets. BIGINT is signed int64 (max 9223372036854775807), so pgx
-- rejected any uint64 > MaxInt64 at PutLock and the lock was never persisted.
-- NUMERIC(20) holds every 20-digit value, covering the full uint64 range.

ALTER TABLE locks
    ALTER COLUMN byte_offset TYPE NUMERIC(20) USING byte_offset::NUMERIC(20),
    ALTER COLUMN byte_length TYPE NUMERIC(20) USING byte_length::NUMERIC(20);
