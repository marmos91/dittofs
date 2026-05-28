ALTER TABLE durable_handles DROP CONSTRAINT IF EXISTS valid_original_file_id;
ALTER TABLE durable_handles DROP COLUMN IF EXISTS original_file_id;
ALTER TABLE durable_handles DROP COLUMN IF EXISTS position_info;
