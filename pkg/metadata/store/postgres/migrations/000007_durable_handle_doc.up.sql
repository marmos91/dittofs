-- Add delete-on-close and directory state fields to durable handles.
-- These fields enable the scavenger to execute delete-on-close when
-- a durable handle expires, and preserve directory state across reconnect.

ALTER TABLE durable_handles ADD COLUMN IF NOT EXISTS delete_pending BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE durable_handles ADD COLUMN IF NOT EXISTS parent_handle BYTEA;
ALTER TABLE durable_handles ADD COLUMN IF NOT EXISTS file_name TEXT NOT NULL DEFAULT '';
ALTER TABLE durable_handles ADD COLUMN IF NOT EXISTS is_directory BOOLEAN NOT NULL DEFAULT FALSE;
