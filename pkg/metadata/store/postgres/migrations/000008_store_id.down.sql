-- Reverse Phase 5 D-06 store_id column addition.
ALTER TABLE server_config DROP COLUMN IF EXISTS store_id;
