-- This column tracks the number of hard links to a file directly in the files table
-- for faster access during GETATTR operations (required by fstat() after unlink())

ALTER TABLE files ADD COLUMN IF NOT EXISTS nlink INTEGER DEFAULT 1;

-- Initialize nlink values from link_counts table
UPDATE files f
SET nlink = COALESCE(
    (SELECT lc.link_count FROM link_counts lc WHERE lc.file_id = f.id),
    1
);

-- Set NOT NULL after initialization
ALTER TABLE files ALTER COLUMN nlink SET NOT NULL;
