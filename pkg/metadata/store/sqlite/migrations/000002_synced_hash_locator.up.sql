-- #1414 object packing (PR3a): record WHERE a synced chunk's bytes live, so a
-- chunk can resolve to either its standalone CAS object (NULL columns, the
-- default for every existing row — no backfill needed) or a position inside a
-- pack object (pack_id/pack_offset/pack_length). Standalone marks leave these
-- NULL, so behavior is unchanged until PR3b's packer writes pack locators.
ALTER TABLE synced_hashes ADD COLUMN pack_id TEXT;
ALTER TABLE synced_hashes ADD COLUMN pack_offset INTEGER;
ALTER TABLE synced_hashes ADD COLUMN pack_length INTEGER;
