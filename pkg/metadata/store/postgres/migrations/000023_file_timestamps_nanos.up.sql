-- Store file timestamps as BIGINT nanoseconds for lossless FILETIME parity.
--
-- TIMESTAMPTZ stores at most microsecond precision, but SMB FILETIME (and the
-- memory/badger backends) carry 100-nanosecond granularity. Setting a sub-
-- microsecond timestamp via SET_INFO then re-querying it returned a truncated
-- value on postgres only, failing WPTS BVT_SMB2Basic_QueryAndSet_FileInfo
-- (the FILETIME assert) while passing on memory/badger which keep full
-- nanosecond fidelity (#882).
--
-- Convert the four files timestamp columns to BIGINT holding unix nanoseconds.
-- Existing rows are already microsecond-truncated, so the EXTRACT conversion is
-- lossless for them; new writes store the full nanosecond value via the Go
-- layer (timeToPGNanos / pgNanosToTime). A NULL/zero time.Time maps to 0
-- nanoseconds, matching the zero-value semantics the scan path reconstructs.
--
-- Range: int64 nanoseconds covers years 1678..2262 (time.Time.UnixNano range).
-- FILETIME values outside that range are already clamped to the zero time by
-- internal/adapter/smb/types.FiletimeToTime, so no SMB-reachable value
-- overflows.

ALTER TABLE files
    ALTER COLUMN atime DROP DEFAULT,
    ALTER COLUMN mtime DROP DEFAULT,
    ALTER COLUMN ctime DROP DEFAULT,
    ALTER COLUMN creation_time DROP DEFAULT;

ALTER TABLE files
    ALTER COLUMN atime TYPE BIGINT
        USING (EXTRACT(EPOCH FROM atime) * 1000000000)::BIGINT,
    ALTER COLUMN mtime TYPE BIGINT
        USING (EXTRACT(EPOCH FROM mtime) * 1000000000)::BIGINT,
    ALTER COLUMN ctime TYPE BIGINT
        USING (EXTRACT(EPOCH FROM ctime) * 1000000000)::BIGINT,
    ALTER COLUMN creation_time TYPE BIGINT
        USING (EXTRACT(EPOCH FROM creation_time) * 1000000000)::BIGINT;

ALTER TABLE files
    ALTER COLUMN atime SET DEFAULT 0,
    ALTER COLUMN mtime SET DEFAULT 0,
    ALTER COLUMN ctime SET DEFAULT 0,
    ALTER COLUMN creation_time SET DEFAULT 0;
