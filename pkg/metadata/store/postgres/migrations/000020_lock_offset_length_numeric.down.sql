-- Revert byte_offset/byte_length to BIGINT (signed int64, max
-- 9223372036854775807). The up migration widened these to NUMERIC(20)
-- specifically so unbounded NFSv4 byte-range locks (Offset/Length =
-- 0xFFFFFFFFFFFFFFFF) and SMB high-bit offsets could be persisted — values that
-- BIGINT cannot represent. A naive ALTER ... USING ::BIGINT would overflow and
-- abort the migration on any such row, making the down step unrunnable.
--
-- Locks are advisory and recoverable: a dropped lock is re-acquired by the
-- client on the next operation (and never survives a server restart past its
-- owner's reconnect). So we DELETE the rows that BIGINT cannot hold before the
-- ALTER, keeping the down migration runnable. The only consequence is that an
-- in-flight unbounded-range lock is forgotten across this schema rollback.

DELETE FROM locks
WHERE byte_offset > 9223372036854775807
   OR byte_length > 9223372036854775807;

ALTER TABLE locks
    ALTER COLUMN byte_offset TYPE BIGINT USING byte_offset::BIGINT,
    ALTER COLUMN byte_length TYPE BIGINT USING byte_length::BIGINT;
