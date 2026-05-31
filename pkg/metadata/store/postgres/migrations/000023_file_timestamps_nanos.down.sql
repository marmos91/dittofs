-- Revert file timestamp columns from BIGINT nanoseconds back to TIMESTAMPTZ.
-- Sub-microsecond precision written while on BIGINT is truncated to microsecond
-- on the way back, matching the original column fidelity.

ALTER TABLE files
    ALTER COLUMN atime DROP DEFAULT,
    ALTER COLUMN mtime DROP DEFAULT,
    ALTER COLUMN ctime DROP DEFAULT,
    ALTER COLUMN creation_time DROP DEFAULT;

ALTER TABLE files
    ALTER COLUMN atime TYPE TIMESTAMPTZ
        USING to_timestamp(atime / 1000000000.0),
    ALTER COLUMN mtime TYPE TIMESTAMPTZ
        USING to_timestamp(mtime / 1000000000.0),
    ALTER COLUMN ctime TYPE TIMESTAMPTZ
        USING to_timestamp(ctime / 1000000000.0),
    ALTER COLUMN creation_time TYPE TIMESTAMPTZ
        USING to_timestamp(creation_time / 1000000000.0);

ALTER TABLE files
    ALTER COLUMN atime SET DEFAULT NOW(),
    ALTER COLUMN mtime SET DEFAULT NOW(),
    ALTER COLUMN ctime SET DEFAULT NOW(),
    ALTER COLUMN creation_time SET DEFAULT NOW();
