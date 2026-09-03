-- Create the filesystem_meta table the share queries have always read and
-- written. It was present in the sqlite schema from the start but never
-- created here, so GetFilesystemMeta failed with an undefined-relation error
-- on every call and PutFilesystemMeta persisted nothing.
--
-- Shape mirrors sqlite: one row per share holding the JSON-encoded
-- metadata.FilesystemMeta blob.
CREATE TABLE IF NOT EXISTS filesystem_meta (
    share_name TEXT PRIMARY KEY,
    meta       TEXT NOT NULL DEFAULT '{}'
);
