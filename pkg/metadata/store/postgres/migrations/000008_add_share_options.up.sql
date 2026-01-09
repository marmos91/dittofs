-- Add options column to shares table for storing share configuration as JSON
ALTER TABLE shares ADD COLUMN IF NOT EXISTS options JSONB NOT NULL DEFAULT '{}'::jsonb;
