-- #1414 object packing (PR3a): record WHERE a synced chunk's bytes live, so a
-- chunk can resolve to either its standalone CAS object (NULL columns, the
-- default for every existing row — no backfill needed) or a position inside a
-- block object (block_id/block_offset/block_length). Standalone marks leave these
-- NULL, so behavior is unchanged until PR3b's packer writes block locators.
ALTER TABLE synced_hashes ADD COLUMN IF NOT EXISTS block_id TEXT;
ALTER TABLE synced_hashes ADD COLUMN IF NOT EXISTS block_offset BIGINT;
ALTER TABLE synced_hashes ADD COLUMN IF NOT EXISTS block_length BIGINT;
