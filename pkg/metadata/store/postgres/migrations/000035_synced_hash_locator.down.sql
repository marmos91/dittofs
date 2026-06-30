ALTER TABLE synced_hashes DROP COLUMN IF EXISTS block_length;
ALTER TABLE synced_hashes DROP COLUMN IF EXISTS block_offset;
ALTER TABLE synced_hashes DROP COLUMN IF EXISTS block_id;
