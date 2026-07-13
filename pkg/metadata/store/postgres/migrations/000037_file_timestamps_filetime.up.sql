-- Re-encode inode file timestamps from unix-nanoseconds to Windows FILETIME
-- (signed 100ns ticks since 1601-01-01). BIGINT unix-nanoseconds overflows past
-- year 2262 and conflates the unix epoch (0 ns) with the zero-time sentinel;
-- FILETIME spans years 1601–~30828 and keeps epoch distinct, so extreme SMB
-- timestamps round-trip on par with the memory/badger backends (#1663). This
-- supersedes the "no SMB-reachable value overflows" range caveat in 000023 —
-- smbtorture smb2.timestamps.time_t_{10000000000,15032385535} reach the store.
--
-- 116444736000000000 = 100ns ticks between 1601-01-01 and 1970-01-01. A stored
-- 0 stays 0 (the "unset"/zero-time sentinel the Go layer maps to time.Time{}).
-- Integer division truncates the sub-100ns remainder, matching timeToPGFiletime.
UPDATE inodes SET
    atime         = CASE WHEN atime = 0 THEN 0 ELSE atime / 100 + 116444736000000000 END,
    mtime         = CASE WHEN mtime = 0 THEN 0 ELSE mtime / 100 + 116444736000000000 END,
    ctime         = CASE WHEN ctime = 0 THEN 0 ELSE ctime / 100 + 116444736000000000 END,
    creation_time = CASE WHEN creation_time = 0 THEN 0 ELSE creation_time / 100 + 116444736000000000 END,
    deleted_at    = CASE WHEN deleted_at IS NULL OR deleted_at = 0 THEN deleted_at
                         ELSE deleted_at / 100 + 116444736000000000 END;
