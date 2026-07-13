-- Revert inode file timestamps from Windows FILETIME back to unix-nanoseconds.
-- Lossy only for pre-1678 / post-2262 values the nanosecond encoding cannot
-- represent (those were unreachable before #1663).
UPDATE inodes SET
    atime         = CASE WHEN atime = 0 THEN 0 ELSE (atime - 116444736000000000) * 100 END,
    mtime         = CASE WHEN mtime = 0 THEN 0 ELSE (mtime - 116444736000000000) * 100 END,
    ctime         = CASE WHEN ctime = 0 THEN 0 ELSE (ctime - 116444736000000000) * 100 END,
    creation_time = CASE WHEN creation_time = 0 THEN 0 ELSE (creation_time - 116444736000000000) * 100 END,
    deleted_at    = CASE WHEN deleted_at IS NULL OR deleted_at = 0 THEN deleted_at
                         ELSE (deleted_at - 116444736000000000) * 100 END;
