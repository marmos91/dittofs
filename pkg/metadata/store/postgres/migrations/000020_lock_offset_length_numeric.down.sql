-- Revert byte_offset/byte_length to BIGINT. Values above MaxInt64 (unbounded
-- uint64 ranges) cannot be represented and would overflow; this down migration
-- assumes no such rows exist.

ALTER TABLE locks
    ALTER COLUMN byte_offset TYPE BIGINT USING byte_offset::BIGINT,
    ALTER COLUMN byte_length TYPE BIGINT USING byte_length::BIGINT;
