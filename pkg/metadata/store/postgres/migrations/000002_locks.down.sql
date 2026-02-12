-- Rollback: Remove locks table
DROP INDEX IF EXISTS idx_locks_share_name;
DROP INDEX IF EXISTS idx_locks_client_id;
DROP INDEX IF EXISTS idx_locks_owner_id;
DROP INDEX IF EXISTS idx_locks_file_id;
DROP TABLE IF EXISTS locks;
DROP TABLE IF EXISTS server_epoch;
