ALTER TABLE synced_hashes DROP COLUMN IF EXISTS pack_length;
ALTER TABLE synced_hashes DROP COLUMN IF EXISTS pack_offset;
ALTER TABLE synced_hashes DROP COLUMN IF EXISTS pack_id;
