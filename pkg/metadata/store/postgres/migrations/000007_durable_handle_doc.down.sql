ALTER TABLE durable_handles DROP COLUMN IF EXISTS delete_pending;
ALTER TABLE durable_handles DROP COLUMN IF EXISTS parent_handle;
ALTER TABLE durable_handles DROP COLUMN IF EXISTS file_name;
ALTER TABLE durable_handles DROP COLUMN IF EXISTS is_directory;
