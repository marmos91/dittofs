-- Add creation_time column to files table for SMB and NFSv4 compatibility
-- This stores the actual file creation time (birth time), distinct from ctime
-- which tracks metadata changes in Unix

ALTER TABLE files ADD COLUMN creation_time TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- Backfill existing rows: set creation_time to ctime for existing files
-- This is a reasonable default since ctime is the closest we have to creation time
UPDATE files SET creation_time = ctime;
