-- Convert the files timestamp columns from unix-nanoseconds to Windows FILETIME
-- (100ns ticks since 1601), matching the encoding.go switch (timeToNanos /
-- nanosToTime). Zero stays zero (the unset/zero-time sentinel); a nonzero
-- unix-nanosecond value n becomes n/100 + 116444736000000000. On a fresh
-- database these tables are empty so this is a no-op; on an existing database it
-- rewrites the persisted values so they decode correctly under the new codec.
UPDATE inodes SET
    atime         = CASE WHEN atime         = 0 THEN 0 ELSE atime / 100         + 116444736000000000 END,
    mtime         = CASE WHEN mtime         = 0 THEN 0 ELSE mtime / 100         + 116444736000000000 END,
    ctime         = CASE WHEN ctime         = 0 THEN 0 ELSE ctime / 100         + 116444736000000000 END,
    creation_time = CASE WHEN creation_time = 0 THEN 0 ELSE creation_time / 100 + 116444736000000000 END,
    deleted_at    = CASE WHEN deleted_at IS NULL OR deleted_at = 0 THEN deleted_at ELSE deleted_at / 100 + 116444736000000000 END;
