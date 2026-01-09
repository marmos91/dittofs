-- Rollback initial schema for DittoFS PostgreSQL metadata store
-- Drops all tables, functions, and triggers in reverse order

-- Drop triggers
DROP TRIGGER IF EXISTS update_server_config_updated_at ON server_config;
DROP TRIGGER IF EXISTS update_files_updated_at ON files;

-- Drop function
DROP FUNCTION IF EXISTS update_updated_at_column();

-- Drop tables (cascade will handle dependencies)
DROP TABLE IF EXISTS filesystem_capabilities CASCADE;
DROP TABLE IF EXISTS server_config CASCADE;
DROP TABLE IF EXISTS pending_writes CASCADE;
DROP TABLE IF EXISTS shares CASCADE;
DROP TABLE IF EXISTS link_counts CASCADE;
DROP TABLE IF EXISTS parent_child_map CASCADE;
DROP TABLE IF EXISTS files CASCADE;

-- Note: We don't drop the uuid-ossp extension as it might be used by other tables
-- DROP EXTENSION IF EXISTS "uuid-ossp";
