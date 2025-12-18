-- Add hidden column for SMB/Windows hidden file attribute support
-- Hidden files are not displayed in standard directory listings on Windows

ALTER TABLE files ADD COLUMN hidden BOOLEAN NOT NULL DEFAULT FALSE;

-- Create index for efficient filtering of hidden files
CREATE INDEX idx_files_hidden ON files(hidden) WHERE hidden = TRUE;
