-- Add ACL column to files table (JSONB for flexible ACL storage)
ALTER TABLE files ADD COLUMN IF NOT EXISTS acl JSONB DEFAULT NULL;

-- Partial index for files with ACLs (optimize queries that check ACL presence)
CREATE INDEX IF NOT EXISTS idx_files_has_acl ON files ((acl IS NOT NULL)) WHERE acl IS NOT NULL;
