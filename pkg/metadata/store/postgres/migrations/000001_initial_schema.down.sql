-- Rollback: Drop all tables, functions, triggers, and extensions
-- Order matters: drop dependent objects first

-- Drop triggers
DROP TRIGGER IF EXISTS files_content_id_hash_trigger ON files;
DROP TRIGGER IF EXISTS files_path_hash_trigger ON files;
DROP TRIGGER IF EXISTS update_server_config_updated_at ON server_config;
DROP TRIGGER IF EXISTS update_files_updated_at ON files;

-- Drop functions
DROP FUNCTION IF EXISTS update_content_id_hash();
DROP FUNCTION IF EXISTS update_path_hash();
DROP FUNCTION IF EXISTS update_updated_at_column();

-- Drop tables (order matters due to foreign key constraints)
DROP TABLE IF EXISTS filesystem_capabilities;
DROP TABLE IF EXISTS server_config;
DROP TABLE IF EXISTS pending_writes;
DROP TABLE IF EXISTS shares;
DROP TABLE IF EXISTS link_counts;
DROP TABLE IF EXISTS parent_child_map;
DROP TABLE IF EXISTS files;

-- Note: We don't drop the uuid-ossp extension as it may be used by other databases
